"""SQLite storage.

Two database files:
  * CACHE_DIR/library.db  - everything derived from the media (scan + probe
    results, TMDB/AniList metadata). Disposable: delete it and the next scan rebuilds it.
  * CONFIG_DIR/state.db   - watch progress. Small, keep it.

Item ids are a hash of (library name, path relative to the library root), so
progress survives a cache wipe and a change of mount point.
"""

from __future__ import annotations

import contextlib
import hashlib
import os
import queue
import sqlite3
import threading

LIBRARY_SCHEMA = """
CREATE TABLE IF NOT EXISTS items (
    id TEXT PRIMARY KEY,
    library TEXT NOT NULL,
    rel_path TEXT NOT NULL,
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    mtime REAL NOT NULL,
    kind TEXT NOT NULL,
    series_id TEXT,
    season INTEGER,
    episode INTEGER,
    episode_end INTEGER,
    title TEXT NOT NULL,
    year INTEGER,
    sort_name TEXT NOT NULL,
    added_at REAL NOT NULL,
    image TEXT,
    probed INTEGER NOT NULL DEFAULT 0,
    probe_error TEXT,
    duration REAL,
    start_time REAL,
    container TEXT,
    video_codec TEXT,
    width INTEGER,
    height INTEGER,
    pix_fmt TEXT,
    streams TEXT,
    skips TEXT
);
CREATE INDEX IF NOT EXISTS items_series ON items(series_id, season, episode);
CREATE INDEX IF NOT EXISTS items_library ON items(library, kind);
CREATE INDEX IF NOT EXISTS items_probed ON items(probed);
CREATE INDEX IF NOT EXISTS items_added ON items(added_at);
CREATE TABLE IF NOT EXISTS series (
    id TEXT PRIMARY KEY,
    library TEXT NOT NULL,
    title TEXT NOT NULL,
    year INTEGER,
    dir TEXT,
    poster TEXT,
    sort_name TEXT NOT NULL,
    added_at REAL NOT NULL,
    missing INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS metadata (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    query TEXT NOT NULL,
    tmdb_id INTEGER,
    anilist_id INTEGER,
    title TEXT,
    year INTEGER,
    overview TEXT,
    poster_url TEXT,
    art_at REAL NOT NULL,
    fetched_at REAL NOT NULL
);
"""

STATE_SCHEMA = """
CREATE TABLE IF NOT EXISTS st.progress (
    id TEXT PRIMARY KEY,
    position REAL NOT NULL DEFAULT 0,
    duration REAL,
    watched INTEGER NOT NULL DEFAULT 0,
    updated_at REAL NOT NULL
);
"""


def item_id(library: str, rel_path: str) -> str:
    return hashlib.sha1(f"{library}\0{rel_path}".encode()).hexdigest()[:16]


def series_id(library: str, key: str) -> str:
    return hashlib.sha1(f"series\0{library}\0{key}".encode()).hexdigest()[:16]


# A few shared connections instead of one per request thread. More readers
# than cores only adds GIL contention on row fetches, and every connection
# that is opened (with its page cache) is memory that is never handed back.
POOL_SIZE = max(2, min(4, os.cpu_count() or 1))


class Database:
    def __init__(self, cache_dir: str, config_dir: str, pool_size: int = POOL_SIZE):
        os.makedirs(cache_dir, exist_ok=True)
        os.makedirs(config_dir, exist_ok=True)
        self.library_path = os.path.join(cache_dir, "library.db")
        self.state_path = os.path.join(config_dir, "state.db")
        self._pool: queue.LifoQueue[sqlite3.Connection] = queue.LifoQueue()
        self._pool_size = max(1, pool_size)
        self._opened = 0
        self._pool_lock = threading.Lock()
        self.write_lock = threading.Lock()
        # Bumped after every committed write. The JSON views use it as their
        # ETag, so a revisit with nothing changed costs no query at all.
        self.generation = 0
        with self.conn() as c:
            c.executescript(LIBRARY_SCHEMA)
            c.executescript(STATE_SCHEMA)
            cols = {r["name"] for r in c.execute("PRAGMA table_info(items)")}
            if "skips" not in cols:
                # Intro/credits chapters arrived later: probe everything once
                # more so existing libraries get them too.
                c.execute("ALTER TABLE items ADD COLUMN skips TEXT")
                c.execute("UPDATE items SET probed=0 WHERE probed=1")
                c.commit()
            series_cols = {r["name"] for r in c.execute("PRAGMA table_info(series)")}
            if "missing" not in series_cols:
                # 1 = an empty show folder, kept as a series with no episodes.
                c.execute("ALTER TABLE series ADD COLUMN missing INTEGER NOT NULL DEFAULT 0")
                c.commit()
            meta_cols = {r["name"] for r in c.execute("PRAGMA table_info(metadata)")}
            if "anilist_id" not in meta_cols:
                c.execute("ALTER TABLE metadata ADD COLUMN anilist_id INTEGER")
                c.commit()

    def _open(self) -> sqlite3.Connection:
        c = sqlite3.connect(self.library_path, timeout=30, check_same_thread=False)
        c.row_factory = sqlite3.Row
        c.execute("PRAGMA journal_mode=WAL")
        c.execute("PRAGMA synchronous=NORMAL")
        c.execute("PRAGMA temp_store=MEMORY")
        c.execute("PRAGMA cache_size=-4000")  # 4 MB per connection; the hot set is the indexes
        c.execute("ATTACH DATABASE ? AS st", (self.state_path,))
        c.execute("PRAGMA st.journal_mode=WAL")
        return c

    @contextlib.contextmanager
    def conn(self):
        """Borrow a pooled connection; waits when all of them are in use."""
        try:
            c = self._pool.get_nowait()
        except queue.Empty:
            with self._pool_lock:
                grow = self._opened < self._pool_size
                if grow:
                    self._opened += 1
            if grow:
                try:
                    c = self._open()
                except BaseException:
                    with self._pool_lock:
                        self._opened -= 1
                    raise
            else:
                c = self._pool.get()
        try:
            yield c
        finally:
            if c.in_transaction:
                c.rollback()
            self._pool.put(c)

    def query(self, sql: str, args=()) -> list[sqlite3.Row]:
        with self.conn() as c:
            return c.execute(sql, args).fetchall()

    def one(self, sql: str, args=()) -> sqlite3.Row | None:
        with self.conn() as c:
            cur = c.execute(sql, args)
            try:
                return cur.fetchone()
            finally:
                cur.close()  # don't hand the connection on with a half-read statement

    def write(self, sql: str, args=()) -> None:
        with self.write_lock, self.conn() as c:
            c.execute(sql, args)
            c.commit()
            self.generation += 1

    def write_many(self, statements) -> None:
        """Run [(sql, args), ...] in a single transaction."""
        with self.write_lock, self.conn() as c:
            try:
                for sql, args in statements:
                    c.execute(sql, args)
                c.commit()
                self.generation += 1
            except BaseException:
                c.rollback()
                raise
