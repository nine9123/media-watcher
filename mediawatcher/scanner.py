"""Library scanning: walk, parse names, probe new files, clean up removed ones."""

from __future__ import annotations

import ctypes
import json
import logging
import os
import threading
import time
from concurrent.futures import ThreadPoolExecutor

from . import naming, probe
from .config import Config, Library
from .db import Database, item_id, series_id
from .images import POSTER_NAMES, Images, find_image

log = logging.getLogger("mediawatcher.scan")


def release_memory() -> None:
    """Hand freed heap back to the OS. glibc keeps it otherwise, so a scan's
    row dicts would stay in RSS. A no-op where there is no glibc (musl, macOS)."""
    try:
        ctypes.CDLL("libc.so.6").malloc_trim(0)
    except (OSError, AttributeError):
        pass


def walk_library(root: str):
    """Yield (rel_dir, listing, video_names) for every directory under root.

    `listing` maps lower-case name -> real name (used to find sidecar images).
    Unreadable directories are skipped, not fatal: a NAS share with one
    broken folder must not take the whole library down.
    """
    stack = [""]
    while stack:
        rel = stack.pop()
        full = os.path.join(root, rel) if rel else root
        try:
            entries = list(os.scandir(full))
        except OSError as e:
            log.warning("cannot read %s: %s", full, e)
            continue
        listing: dict[str, str] = {}
        videos: list[str] = []
        for e in entries:
            name = e.name
            try:
                is_dir = e.is_dir()
            except OSError:
                continue
            if is_dir:
                if not naming.is_ignored_dir(name):
                    stack.append(f"{rel}/{name}" if rel else name)
                continue
            listing[name.lower()] = name
            if naming.is_video(name) and not name.startswith(".") and not naming.is_extra(name):
                videos.append(name)
        yield rel, listing, videos


def merge_series_dirs(series: dict[str, dict]) -> dict[str, str]:
    """Map series-folder keys that name the same show onto one canonical key.

    Folders merge when their cleaned titles match and their years are equal.
    A folder with no year joins the one dated folder of that title, but stays
    on its own if there are several (remakes: "Show (2005)", "Show (2020)").
    """
    groups: dict[tuple[str, int | None], list[str]] = {}
    for key, s in series.items():
        groups.setdefault((naming.title_key(s["title"]), s["year"]), []).append(key)
    for (tkey, year), keys in list(groups.items()):
        if year is not None:
            continue
        dated = [g for g in groups if g[0] == tkey and g[1] is not None]
        if len(dated) == 1:
            groups[dated[0]].extend(groups.pop((tkey, year)))
    merge: dict[str, str] = {}
    for keys in groups.values():
        if len(keys) > 1:
            keys.sort(key=lambda k: (series[k]["year"] is None, k))
            for key in keys[1:]:
                merge[key] = keys[0]
    return merge


class Scanner:
    def __init__(self, cfg: Config, db: Database, images: Images):
        self.cfg = cfg
        self.db = db
        self.images = images
        self.status: dict = {"running": False, "phase": "idle", "files": 0, "to_probe": 0,
                             "probed": 0, "last_scan": None, "last_duration": None,
                             "error": None}
        self._run_lock = threading.Lock()
        self._wake = threading.Event()
        self._retry_errors = False

    # ---- background loop -------------------------------------------------
    def start(self) -> None:
        threading.Thread(target=self._loop, name="scanner", daemon=True).start()

    def trigger(self, retry_errors: bool = True) -> None:
        self._retry_errors = self._retry_errors or retry_errors
        self._wake.set()

    def _loop(self) -> None:
        while True:
            try:
                self.scan()
            except Exception as e:  # keep the loop alive no matter what
                log.exception("scan failed")
                self.status["error"] = str(e)
            timeout = self.cfg.scan_interval if self.cfg.scan_interval > 0 else None
            self._wake.wait(timeout)
            self._wake.clear()

    # ---- scanning --------------------------------------------------------
    def scan(self) -> None:
        if not self._run_lock.acquire(blocking=False):
            return
        try:
            t0 = time.time()
            retry = self._retry_errors
            self._retry_errors = False
            self.status.update(running=True, phase="walking", files=0, probed=0,
                               to_probe=0, error=None)
            for lib in self.cfg.libraries:
                self.scan_library(lib)
            self._drop_unknown_libraries()
            self.status["phase"] = "probing"
            self.probe_pending(retry_errors=retry)
            self.status["phase"] = "thumbnails"
            self.pregenerate_images()
            self.status.update(last_scan=time.time(), last_duration=round(time.time() - t0, 1))
        finally:
            self.status.update(running=False, phase="idle")
            self._run_lock.release()
            release_memory()

    def scan_library(self, lib: Library) -> None:
        root = lib.path
        if not os.path.isdir(root):
            log.warning("library %s: %s is missing, keeping existing entries", lib.name, root)
            self.status["error"] = f"{lib.name}: {root} not found"
            return

        existing = {
            r["id"]: r for r in self.db.query(
                "SELECT id, size, mtime, added_at, probed, image FROM items WHERE library=?",
                (lib.name,))
        }
        old_posters = {r["id"]: r["poster"] for r in self.db.query(
            "SELECT id, poster FROM series WHERE library=?", (lib.name,))}
        dirs: dict[str, dict[str, str]] = {}
        files: list[tuple[str, str, list[str]]] = []
        for rel_dir, listing, videos in walk_library(root):
            dirs[rel_dir] = listing
            if videos:
                files.append((rel_dir, listing, videos))

        parsed_files = []
        for rel_dir, listing, videos in files:
            for name in videos:
                rel = f"{rel_dir}/{name}" if rel_dir else name
                parsed_files.append((rel_dir, listing, videos, name, rel,
                                     naming.parse_path(rel, lib.type, siblings=len(videos))))
        # A show folder with recognised episodes claims every other video under
        # it too ("Show/Pilot.mkv", "Show/Arc 2/Some Title.mkv"), so those don't
        # end up as loose movies cluttering the library.
        show_dirs = {p.series_dir for *_, p in parsed_files if p.kind == "episode" and p.series_dir}
        if lib.type == "auto" and show_dirs:
            for i, (rel_dir, listing, videos, name, rel, parsed) in enumerate(parsed_files):
                if parsed.kind == "movie" and rel_dir and rel_dir.split("/")[0] in show_dirs:
                    parsed = naming.parse_path(rel, "shows", siblings=len(videos))
                    parsed_files[i] = (rel_dir, listing, videos, name, rel, parsed)

        rows = []
        series: dict[str, dict] = {}
        loose: dict[str, list[dict]] = {}
        for rel_dir, listing, videos, name, rel, parsed in parsed_files:
            full = os.path.join(root, rel)
            try:
                st = os.stat(full)
            except OSError:
                continue
            iid = item_id(lib.name, rel)
            stem = os.path.splitext(name)[0]
            row = {
                "id": iid, "library": lib.name, "rel_path": rel, "path": full,
                "size": st.st_size, "mtime": st.st_mtime, "kind": parsed.kind,
                "season": parsed.season, "episode": parsed.episode,
                "episode_end": parsed.episode_end, "title": parsed.title,
                "year": parsed.year, "series_id": None,
            }
            if parsed.kind == "episode":
                img = find_image(listing, (f"{stem}-thumb", stem))
                s_key = parsed.series_dir or ""
                s = {"title": parsed.series_title, "year": parsed.series_year,
                     "dir": parsed.series_dir}
                if s_key:
                    series.setdefault(s_key, s)
                else:
                    loose.setdefault(naming.title_key(parsed.series_title), []).append(row)
                row["_series_key"] = s_key
                row["_series"] = s
                row["sort_name"] = f"{(parsed.season or 0):04d}{(parsed.episode or 0):05d}{name.lower()}"
                if not row["title"]:
                    row["title"] = f"Episode {parsed.episode}" if parsed.episode is not None else stem
            else:
                img = find_image(listing, (f"{stem}-poster", stem, f"{stem}-thumb"))
                if img is None and rel_dir and len(videos) == 1:
                    img = find_image(listing, POSTER_NAMES)
                row["sort_name"] = naming.sort_key(parsed.title)
            row["image"] = os.path.join(root, rel_dir, img) if img else None
            old = existing.get(iid)
            row["changed"] = old is None or old["size"] != st.st_size or old["mtime"] != st.st_mtime
            row["added_at"] = old["added_at"] if old else max(st.st_mtime, st.st_ctime)
            rows.append(row)

        # Several top-level folders of one show ("Show.S01.1080p-GRP",
        # "Show.S02.1080p-GRP") become one series. A folder without a year
        # joins the folder with a year only when there is no other candidate.
        merge = merge_series_dirs(series)
        if merge:
            for row in rows:
                if row["kind"] == "episode" and row["_series_key"] in merge:
                    row["_series_key"] = merge[row["_series_key"]]
            for key, target in merge.items():
                series[target].setdefault("dirs", [target]).append(series.pop(key)["dir"])

        # Loose episode files ("[Grp] Show - 01.mkv" in the library root) join a
        # folder series with the same title when there is exactly one.
        by_title: dict[str, list[str]] = {}
        for key, s in series.items():
            by_title.setdefault(naming.title_key(s["title"]), []).append(key)
        for tkey, members in loose.items():
            targets = by_title.get(tkey, [])
            for row in members:
                if len(targets) == 1:
                    row["_series_key"] = targets[0]
                else:
                    row["_series_key"] = "loose:" + tkey
                    series.setdefault(row["_series_key"], row["_series"])

        stmts = []
        now = time.time()
        series_ids = {}
        posters = {}
        for key, s in series.items():
            sid = series_id(lib.name, key)
            series_ids[key] = sid
            poster = None
            for d in s.get("dirs", [s["dir"]] if s["dir"] else []):
                img = find_image(dirs.get(d, {}), POSTER_NAMES)
                if img:
                    poster = os.path.join(root, d, img)
                    break
            posters[sid] = poster
            stmts.append((
                "INSERT INTO series(id, library, title, year, dir, poster, sort_name, added_at) "
                "VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET title=excluded.title, "
                "year=excluded.year, dir=excluded.dir, poster=excluded.poster, "
                "sort_name=excluded.sort_name",
                (sid, lib.name, s["title"], s["year"], s["dir"], poster,
                 naming.sort_key(s["title"]), now),
            ))

        for row in rows:
            if row["kind"] == "episode":
                row["series_id"] = series_ids[row["_series_key"]]
            cols = ("id", "library", "rel_path", "path", "size", "mtime", "kind", "series_id",
                    "season", "episode", "episode_end", "title", "year", "sort_name",
                    "added_at", "image")
            values = tuple(row[c] for c in cols)
            reset_probe = ", probed=0, probe_error=NULL" if row["changed"] else ""
            stmts.append((
                f"INSERT INTO items({','.join(cols)}) VALUES({','.join('?' * len(cols))}) "
                "ON CONFLICT(id) DO UPDATE SET path=excluded.path, size=excluded.size, "
                "mtime=excluded.mtime, kind=excluded.kind, series_id=excluded.series_id, "
                "season=excluded.season, episode=excluded.episode, "
                "episode_end=excluded.episode_end, title=excluded.title, year=excluded.year, "
                f"sort_name=excluded.sort_name, image=excluded.image{reset_probe}",
                values,
            ))

        # Auto-cleanup. A share that is mounted but empty (NAS asleep, SMB
        # hiccup) must not wipe the library, so only delete when we saw files.
        seen = {r["id"] for r in rows}
        removed = [i for i in existing if i not in seen] if rows else []
        for iid in removed:
            stmts.append(("DELETE FROM items WHERE id=?", (iid,)))
        self.db.write_many(stmts)
        for iid in removed:
            self.images.remove("item", iid)
        self._drop_stale_images(rows, existing, posters, old_posters)
        self._drop_empty_series()
        self.status["files"] += len(rows)
        if removed:
            log.info("library %s: removed %d missing files", lib.name, len(removed))

    def _drop_stale_images(self, rows, existing, posters, old_posters) -> None:
        """Cached images whose source changed since they were made."""
        for row in rows:
            old = existing.get(row["id"])
            if old is not None and (row["changed"] or old["image"] != row["image"]):
                self.images.remove("item", row["id"])
            else:
                self.images.drop_stale("item", row["id"], row["image"])
        for sid, poster in posters.items():
            if sid in old_posters and old_posters[sid] != poster:
                self.images.remove("series", sid)
            else:
                self.images.drop_stale("series", sid, poster)

    def _drop_empty_series(self) -> None:
        gone = self.db.query(
            "SELECT id FROM series WHERE NOT EXISTS (SELECT 1 FROM items WHERE series_id=series.id)")
        if gone:
            self.db.write_many([("DELETE FROM series WHERE id=?", (r["id"],)) for r in gone])
            for r in gone:
                self.images.remove("series", r["id"])

    def _drop_unknown_libraries(self) -> None:
        names = [lib.name for lib in self.cfg.libraries]
        marks = ",".join("?" * len(names)) or "''"
        self.db.write_many([
            (f"DELETE FROM items WHERE library NOT IN ({marks})", names),
            (f"DELETE FROM series WHERE library NOT IN ({marks})", names),
        ])

    # ---- probing ---------------------------------------------------------
    def probe_item(self, iid: str, path: str) -> bool:
        try:
            info = probe.probe(self.cfg.ffprobe, path)
        except Exception as e:  # noqa: BLE001 - recorded on the item
            self.db.write("UPDATE items SET probed=2, probe_error=? WHERE id=?", (str(e)[:500], iid))
            return False
        self.db.write(
            "UPDATE items SET probed=1, probe_error=NULL, duration=?, start_time=?, container=?, "
            "video_codec=?, width=?, height=?, pix_fmt=?, streams=? WHERE id=?",
            (info["duration"], info["start_time"], info["container"], info["video_codec"],
             info["width"], info["height"], info["pix_fmt"], json.dumps(info["streams"]), iid),
        )
        return True

    def probe_pending(self, retry_errors: bool = False) -> None:
        cond = "probed!=1" if retry_errors else "probed=0"
        # Newest first: freshly added episodes become playable soonest.
        todo = self.db.query(f"SELECT id, path FROM items WHERE {cond} ORDER BY added_at DESC")
        self.status["to_probe"] = len(todo)
        if not todo:
            return

        def work(r):
            self.probe_item(r["id"], r["path"])
            self.status["probed"] += 1

        with ThreadPoolExecutor(self.cfg.probe_workers, thread_name_prefix="probe") as ex:
            list(ex.map(work, todo))

    def pregenerate_images(self) -> None:
        """Warm the image cache so the first library visit is instant."""
        for r in self.db.query("SELECT id FROM series ORDER BY sort_name"):
            if not os.path.exists(self.images.cache_path("series", r["id"])):
                self.images.series_image(r["id"])
        for r in self.db.query("SELECT id FROM items ORDER BY added_at DESC"):
            if not os.path.exists(self.images.cache_path("item", r["id"])) \
                    and not self.images.failed("item", r["id"]):
                self.images.item_image(r["id"])
