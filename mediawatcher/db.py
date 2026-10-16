"""SQLite storage.

Two database files:
  * CACHE_DIR/library.db  - everything derived from the media (scan + probe
    results). Disposable: delete it and the next scan rebuilds it.
  * CONFIG_DIR/state.db   - watch progress. Small, keep it.

Item ids are a hash of (library name, path relative to the library root), so
progress survives a cache wipe and a change of mount point.
"""

from __future__ import annotations

import hashlib
import os
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
    streams TEXT
);
CREATE INDEX IF NOT EXISTS items_series ON items(series_id, season, episode);
CREATE INDEX IF NOT EXISTS items_library ON items(library, kind);
CREATE INDEX IF NOT EXISTS items_probed ON items(probed);
CREATE TABLE IF NOT EXISTS series (
    id TEXT PRIMARY KEY,
    library TEXT NOT NULL,
    title TEXT NOT NULL,
    year INTEGER,
    dir TEXT,
    poster TEXT,
    sort_name TEXT NOT NULL,
    added_at REAL NOT NULL
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


class Database:
    def __init__(self, cache_dir: str, config_dir: str):
        os.makedirs(cache_dir, exist_ok=True)
        os.makedirs(config_dir, exist_ok=True)
        self.library_path = os.path.join(cache_dir, "library.db")
        self.state_path = os.path.join(config_dir, "state.db")
        self._local = threading.local()
        self.write_lock = threading.Lock()
        c = self.conn()
        c.executescript(LIBRARY_SCHEMA)
        c.executescript(STATE_SCHEMA)

    def conn(self) -> sqlite3.Connection:
        c = getattr(self._local, "conn", None)
        if c is None:
            c = sqlite3.connect(self.library_path, timeout=30, check_same_thread=False)
            c.row_factory = sqlite3.Row
            c.execute("PRAGMA journal_mode=WAL")
            c.execute("PRAGMA synchronous=NORMAL")
            c.execute("PRAGMA temp_store=MEMORY")
            c.execute("PRAGMA cache_size=-16000")
            c.execute("ATTACH DATABASE ? AS st", (self.state_path,))
            c.execute("PRAGMA st.journal_mode=WAL")
            self._local.conn = c
        return c

    def query(self, sql: str, args=()) -> list[sqlite3.Row]:
        return self.conn().execute(sql, args).fetchall()

    def one(self, sql: str, args=()) -> sqlite3.Row | None:
        return self.conn().execute(sql, args).fetchone()

    def write(self, sql: str, args=()) -> None:
        with self.write_lock:
            c = self.conn()
            c.execute(sql, args)
            c.commit()

    def write_many(self, statements) -> None:
        """Run [(sql, args), ...] in a single transaction."""
        with self.write_lock:
            c = self.conn()
            try:
                for sql, args in statements:
                    c.execute(sql, args)
                c.commit()
            except BaseException:
                c.rollback()
                raise
