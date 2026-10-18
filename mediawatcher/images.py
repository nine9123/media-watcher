"""Posters and thumbnails, cached on the SSD.

Sidecar images next to the media (poster.jpg, folder.jpg, <name>-thumb.jpg)
win, then a poster downloaded from TMDB (series and movies, see metadata.py);
otherwise a frame is grabbed with ffmpeg. Everything is downscaled once
so library grids never pull multi-megabyte posters over SMB.
"""

from __future__ import annotations

import os
import subprocess
import threading

from . import shutdown
from .config import Config
from .db import Database

POSTER_NAMES = ("poster", "folder", "cover", "show", "default")
IMAGE_EXTS = (".jpg", ".jpeg", ".png", ".webp")
POSTER_WIDTH = 360
THUMB_WIDTH = 480
LOCK_STRIPES = 64


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
        self.meta_dir = os.path.join(cfg.cache_dir, "metadata")
        os.makedirs(self.meta_dir, exist_ok=True)
        self._sem = threading.Semaphore(cfg.thumb_workers)
        # A fixed set of striped locks: one per path would grow forever.
        self._locks = [threading.Lock() for _ in range(LOCK_STRIPES)]

    def _lock(self, key: str) -> threading.Lock:
        return self._locks[hash(key) % LOCK_STRIPES]

    def cache_path(self, kind: str, id_: str) -> str:
        return os.path.join(self.dir, f"{kind}-{id_}.jpg")

    def meta_poster_path(self, id_: str) -> str:
        """Full-size poster downloaded by metadata.py for a series or movie."""
        return os.path.join(self.meta_dir, f"{id_}.jpg")

    def meta_poster(self, id_: str) -> str | None:
        path = self.meta_poster_path(id_)
        return path if os.path.exists(path) else None

    def failed_path(self, kind: str, id_: str) -> str:
        """Zero-byte marker: no image could be made from the current source."""
        return os.path.join(self.dir, f"{kind}-{id_}.none")

    def failed(self, kind: str, id_: str) -> bool:
        return os.path.exists(self.failed_path(kind, id_))

    def remove(self, kind: str, id_: str) -> None:
        for path in (self.cache_path(kind, id_), self.failed_path(kind, id_)):
            try:
                os.remove(path)
            except FileNotFoundError:
                pass

    def drop_stale(self, kind: str, id_: str, source: str | None) -> None:
        """Remove a cached image that is older than the sidecar it was made from.

        Cached images are served without a database lookup, so the scanner
        calls this (and remove()) to keep them in step with the media.
        """
        out = self.cache_path(kind, id_)
        failed = self.failed_path(kind, id_)
        if source and any(os.path.exists(p) and not self._fresh(p, source) for p in (out, failed)):
            self.remove(kind, id_)

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
        row = self.db.one("SELECT path, kind, image, duration FROM items WHERE id=?", (item_id,))
        if row is None:
            return None
        out = self.cache_path("item", item_id)
        failed = self.failed_path("item", item_id)
        image = row["image"]
        if not image and row["kind"] == "movie":
            image = self.meta_poster(item_id)
        source = image or row["path"]
        if self._fresh(out, source):
            return out
        with self._lock(out):
            if self._fresh(out, source):
                return out
            if self._fresh(failed, source):
                return None  # tried before and failed; the scanner clears this on change
            if image and os.path.exists(image):
                if self._resize(image, out, THUMB_WIDTH):
                    return out
            if self._frame(row["path"], row["duration"], out):
                return out
            # Not for a share that is briefly unmounted, nor for an ffmpeg
            # killed because we are shutting down.
            if os.path.exists(source) and not shutdown.stopping.is_set():
                with open(failed, "wb"):
                    pass
        return None

    def series_image(self, series_id: str) -> str | None:
        row = self.db.one("SELECT poster FROM series WHERE id=?", (series_id,))
        if row is None:
            return None
        poster = row["poster"] if row["poster"] and os.path.exists(row["poster"]) else None
        poster = poster or self.meta_poster(series_id)
        if poster:
            out = self.cache_path("series", series_id)
            if self._fresh(out, poster):
                return out
            with self._lock(out):
                if self._fresh(out, poster) or self._resize(poster, out, POSTER_WIDTH):
                    return out
        first = self.db.one(
            "SELECT id FROM items WHERE series_id=? ORDER BY season=0, season, episode IS NULL, "
            "episode, sort_name LIMIT 1",
            (series_id,),
        )
        return self.item_image(first["id"]) if first else None
