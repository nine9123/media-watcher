"""Posters and thumbnails, cached on the SSD.

Sidecar images next to the media (poster.jpg, folder.jpg, <name>-thumb.jpg)
win; otherwise a frame is grabbed with ffmpeg. Everything is downscaled once
so library grids never pull multi-megabyte posters over SMB.
"""

from __future__ import annotations

import os
import subprocess
import threading

from .config import Config
from .db import Database

POSTER_NAMES = ("poster", "folder", "cover", "show", "default")
IMAGE_EXTS = (".jpg", ".jpeg", ".png", ".webp")
POSTER_WIDTH = 360
THUMB_WIDTH = 480


def find_image(listing: dict[str, str], stems: tuple[str, ...]) -> str | None:
    """`listing` maps lower-case file name -> real file name for one directory."""
    for stem in stems:
        for ext in IMAGE_EXTS:
            real = listing.get(f"{stem}{ext}".lower())
            if real:
                return real
    return None


class Images:
    def __init__(self, cfg: Config, db: Database):
        self.cfg = cfg
        self.db = db
        self.dir = os.path.join(cfg.cache_dir, "images")
        os.makedirs(self.dir, exist_ok=True)
        self._sem = threading.Semaphore(cfg.thumb_workers)
        self._locks: dict[str, threading.Lock] = {}
        self._locks_guard = threading.Lock()

    def _lock(self, key: str) -> threading.Lock:
        with self._locks_guard:
            return self._locks.setdefault(key, threading.Lock())

    def cache_path(self, kind: str, id_: str) -> str:
        return os.path.join(self.dir, f"{kind}-{id_}.jpg")

    def remove(self, kind: str, id_: str) -> None:
        try:
            os.remove(self.cache_path(kind, id_))
        except FileNotFoundError:
            pass

    def _fresh(self, out: str, source: str) -> bool:
        try:
            return os.stat(out).st_mtime >= os.stat(source).st_mtime
        except OSError:
            return False

    def _run(self, args: list[str], out: str) -> bool:
        tmp = out + ".tmp.jpg"
        with self._sem:
            try:
                r = subprocess.run(args + ["-y", tmp], capture_output=True, timeout=60, check=False)
            except (OSError, subprocess.TimeoutExpired):
                return False
        if r.returncode == 0 and os.path.exists(tmp) and os.path.getsize(tmp) > 0:
            os.replace(tmp, out)
            return True
        try:
            os.remove(tmp)
        except FileNotFoundError:
            pass
        return False

    def _resize(self, src: str, out: str, width: int) -> bool:
        return self._run(
            [self.cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", src,
             "-frames:v", "1", "-vf", f"scale='min({width},iw)':-2", "-q:v", "3"],
            out,
        )

    def _frame(self, path: str, duration: float | None, out: str) -> bool:
        t = (duration or 0) * 0.2 if duration else 30.0
        t = min(t, 600.0)
        ok = self._run(
            [self.cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
             "-ss", f"{t:.2f}", "-i", path, "-map", "0:v:0", "-frames:v", "1",
             "-vf", f"scale={THUMB_WIDTH}:-2,format=yuvj420p", "-q:v", "4"],
            out,
        )
        if not ok and t > 0:  # very short clips / bad seek index
            ok = self._run(
                [self.cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
                 "-i", path, "-map", "0:v:0", "-frames:v", "1",
                 "-vf", f"scale={THUMB_WIDTH}:-2,format=yuvj420p", "-q:v", "4"],
                out,
            )
        return ok

    def item_image(self, item_id: str) -> str | None:
        row = self.db.one("SELECT path, image, duration FROM items WHERE id=?", (item_id,))
        if row is None:
            return None
        out = self.cache_path("item", item_id)
        source = row["image"] or row["path"]
        if self._fresh(out, source):
            return out
        with self._lock(out):
            if self._fresh(out, source):
                return out
            if row["image"] and os.path.exists(row["image"]):
                if self._resize(row["image"], out, THUMB_WIDTH):
                    return out
            if self._frame(row["path"], row["duration"], out):
                return out
        return None

    def series_image(self, series_id: str) -> str | None:
        row = self.db.one("SELECT poster FROM series WHERE id=?", (series_id,))
        if row is None:
            return None
        if row["poster"] and os.path.exists(row["poster"]):
            out = self.cache_path("series", series_id)
            if self._fresh(out, row["poster"]):
                return out
            with self._lock(out):
                if self._fresh(out, row["poster"]) or self._resize(row["poster"], out, POSTER_WIDTH):
                    return out
        first = self.db.one(
            "SELECT id FROM items WHERE series_id=? ORDER BY season=0, season, episode IS NULL, "
            "episode, sort_name LIMIT 1",
            (series_id,),
        )
        return self.item_image(first["id"]) if first else None
