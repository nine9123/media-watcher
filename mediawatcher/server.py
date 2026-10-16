"""HTTP server: JSON API, web UI, direct file serving and ffmpeg streams.

Standard library only. One user, so a thread per connection is plenty; the
heavy lifting happens in ffmpeg/sendfile, not in Python.
"""

from __future__ import annotations

import collections
import email.utils
import gzip
import hashlib
import hmac
import json
import logging
import mimetypes
import os
import re
import subprocess
import threading
import time
import urllib.parse
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import playback, probe
from .config import Config
from .db import Database
from .images import Images
from .scanner import Scanner

log = logging.getLogger("mediawatcher.http")

COOKIE = "mw_session"
WATCHED_RATIO = 0.90
RESUME_MIN = 30.0
CHUNK = 256 * 1024
FILE_TYPES = {".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
              ".mkv": "video/x-matroska"}


class HttpError(Exception):
    def __init__(self, status: int, message: str = ""):
        super().__init__(message)
        self.status = status
        self.message = message or HTTPStatus(status).phrase


class App:
    """Everything the request handler needs, shared across threads."""

    def __init__(self, cfg: Config, db: Database, images: Images, scanner: Scanner):
        self.cfg = cfg
        self.db = db
        self.images = images
        self.scanner = scanner
        self.session_token = hmac.new(cfg.api_key.encode(), b"mediawatcher-session",
                                      hashlib.sha256).hexdigest()
        self.streams: dict[str, subprocess.Popen] = {}
        self.streams_lock = threading.Lock()

    # ---- auth ------------------------------------------------------------
    def authorized(self, headers, query: dict, cookies: dict) -> bool:
        key = headers.get("X-Api-Key") or (query.get("api_key") or [""])[0]
        if key and hmac.compare_digest(key.encode(), self.cfg.api_key.encode()):
            return True
        tok = cookies.get(COOKIE, "")
        return bool(tok) and hmac.compare_digest(tok.encode(), self.session_token.encode())

    # ---- item helpers ----------------------------------------------------
    # Every row costs a GIL round trip in sqlite3, so list views read only the
    # columns item_json() needs (never the large `streams` blob) and as few
    # rows as possible. ITEM_SQL is for single items that are about to play.
    _JOINS = (
        ", s.title AS series_title, p.position AS position, p.watched AS watched, "
        "p.updated_at AS progress_at FROM items i LEFT JOIN series s ON s.id=i.series_id "
        "LEFT JOIN st.progress p ON p.id=i.id"
    )
    ITEM_SQL = "SELECT i.*" + _JOINS
    LIST_SQL = (
        "SELECT i.id, i.kind, i.title, i.year, i.series_id, i.season, i.episode, "
        "i.episode_end, i.duration, i.video_codec, i.height, i.library, i.path, i.size, "
        "i.added_at, i.probe_error" + _JOINS
    )
    EPISODE_ORDER = ("ORDER BY i.season=0, i.season, i.episode IS NULL, i.episode, "
                     "i.sort_name")

    @staticmethod
    def item_json(r) -> dict:
        keys = r.keys()
        return {
            "id": r["id"], "kind": r["kind"], "title": r["title"], "year": r["year"],
            "series_id": r["series_id"],
            "series_title": r["series_title"] if "series_title" in keys else None,
            "season": r["season"], "episode": r["episode"], "episode_end": r["episode_end"],
            "duration": r["duration"], "position": (r["position"] or 0) if "position" in keys else 0,
            "watched": bool(r["watched"]) if "watched" in keys else False,
            "video_codec": r["video_codec"], "height": r["height"], "library": r["library"],
            "file": os.path.basename(r["path"]), "size": r["size"], "added_at": r["added_at"],
            "probe_error": r["probe_error"],
        }

    def get_item(self, iid: str):
        r = self.db.one(self.ITEM_SQL + " WHERE i.id=?", (iid,))
        if r is None:
            raise HttpError(404, "no such item")
        return r

    def ensure_probed(self, r):
        if r["probed"] != 1:
            if not self.scanner.probe_item(r["id"], r["path"]):
                r2 = self.get_item(r["id"])
                raise HttpError(500, f"cannot read file: {r2['probe_error']}")
            r = self.get_item(r["id"])
        return r

    def episodes(self, sid: str):
        return self.db.query(
            self.LIST_SQL + " WHERE i.series_id=? " + self.EPISODE_ORDER, (sid,))

    def episode_order(self, sid: str):
        """Just (id, season, watched) of a series' episodes, in play order."""
        return self.db.query(
            "SELECT i.id, i.season, COALESCE(p.watched,0) AS watched FROM items i "
            "LEFT JOIN st.progress p ON p.id=i.id WHERE i.series_id=? " + self.EPISODE_ORDER,
            (sid,))

    def neighbours(self, r) -> tuple[str | None, str | None]:
        if not r["series_id"]:
            return None, None
        ids = [e["id"] for e in self.episode_order(r["series_id"])]
        i = ids.index(r["id"])
        return (ids[i - 1] if i > 0 else None, ids[i + 1] if i + 1 < len(ids) else None)

    # ---- progress --------------------------------------------------------
    def save_progress(self, iid: str, position: float, duration: float | None,
                      ended: bool = False) -> dict:
        r = self.get_item(iid)
        duration = duration or r["duration"]
        done = ended or bool(duration and position >= duration * WATCHED_RATIO)
        now = time.time()
        if done:
            self.db.write(
                "INSERT INTO st.progress(id, position, duration, watched, updated_at) "
                "VALUES(?,0,?,1,?) ON CONFLICT(id) DO UPDATE SET position=0, watched=1, "
                "duration=excluded.duration, updated_at=excluded.updated_at",
                (iid, duration, now))
        else:
            self.db.write(
                "INSERT INTO st.progress(id, position, duration, watched, updated_at) "
                "VALUES(?,?,?,0,?) ON CONFLICT(id) DO UPDATE SET position=excluded.position, "
                "duration=excluded.duration, updated_at=excluded.updated_at",
                (iid, max(0.0, position), duration, now))
        return {"watched": done}

    def set_watched(self, ids: list[str], watched: bool) -> None:
        now = time.time()
        self.db.write_many([(
            "INSERT INTO st.progress(id, position, watched, updated_at) VALUES(?,0,?,?) "
            "ON CONFLICT(id) DO UPDATE SET position=0, watched=excluded.watched, "
            "updated_at=excluded.updated_at", (i, 1 if watched else 0, now)) for i in ids])

    @staticmethod
    def resume_point(r) -> float:
        pos = r["position"] or 0
        if r["watched"] or pos < RESUME_MIN:
            return 0.0
        if r["duration"] and pos > r["duration"] * WATCHED_RATIO:
            return 0.0
        return pos

    # ---- views -----------------------------------------------------------
    def home(self) -> dict:
        cont = self.db.query(
            self.LIST_SQL + " WHERE p.watched=0 AND p.position>=? ORDER BY p.updated_at DESC "
            "LIMIT 24", (RESUME_MIN,))
        cont_ids = {r["id"] for r in cont}
        cont_series = {r["series_id"] for r in cont if r["series_id"]}

        # Next up: for recently active series, the episode after the furthest watched one.
        active = self.db.query(
            "SELECT i.series_id AS sid, MAX(p.updated_at) AS t FROM items i "
            "JOIN st.progress p ON p.id=i.id WHERE i.series_id IS NOT NULL "
            "GROUP BY i.series_id ORDER BY t DESC LIMIT 30")
        next_up = []
        for a in active:
            if a["sid"] in cont_series:
                continue
            eps = [e for e in self.episode_order(a["sid"]) if e["season"] != 0]
            last = max((n for n, e in enumerate(eps) if e["watched"]), default=None)
            if last is None:
                continue
            nxt = next((e for e in eps[last + 1:] if not e["watched"]), None)
            if nxt is not None and nxt["id"] not in cont_ids:
                next_up.append(self.item_json(
                    self.db.one(self.LIST_SQL + " WHERE i.id=?", (nxt["id"],))))
            if len(next_up) >= 24:
                break

        # Recently added: newest series (by their newest episode) and movies,
        # grouped in SQL so only the 24 rows shown come back to Python.
        groups = self.db.query(
            "SELECT g.series_id, g.id, g.added_at, s.title FROM ("
            "SELECT series_id, id, MAX(added_at) AS added_at FROM items "
            "GROUP BY COALESCE(series_id, id)) g LEFT JOIN series s ON s.id=g.series_id "
            "ORDER BY g.added_at DESC LIMIT 24")
        movie_ids = [g["id"] for g in groups if not g["series_id"]]
        movies = {r["id"]: r for r in self.db.query(
            self.LIST_SQL + f" WHERE i.id IN ({','.join('?' * len(movie_ids))})",
            movie_ids)} if movie_ids else {}
        recent = []
        for g in groups:
            if g["series_id"]:
                recent.append({"type": "series", "id": g["series_id"],
                               "title": g["title"], "added_at": g["added_at"]})
            elif g["id"] in movies:
                recent.append({"type": "movie", **self.item_json(movies[g["id"]])})
        return {
            "continue": [self.item_json(r) for r in cont],
            "next_up": next_up,
            "recent": recent,
            "libraries": self.libraries(),
        }

    def libraries(self) -> list[dict]:
        counts = {r["library"]: r for r in self.db.query(
            "SELECT library, COUNT(DISTINCT series_id) AS series, "
            "SUM(kind='movie') AS movies FROM items GROUP BY library")}
        out = []
        for lib in self.cfg.libraries:
            c = counts.get(lib.name)
            out.append({"name": lib.name, "type": lib.type,
                        "series": c["series"] if c else 0, "movies": c["movies"] if c else 0})
        return out

    def library(self, name: str) -> dict:
        if name not in {lib.name for lib in self.cfg.libraries}:
            raise HttpError(404, "no such library")
        series = self.db.query(
            "SELECT s.id, s.title, s.year, s.sort_name, COUNT(i.id) AS episodes, "
            "SUM(COALESCE(p.watched,0)) AS watched, MAX(i.added_at) AS added_at, "
            "MAX(p.updated_at) AS played_at FROM series s JOIN items i ON i.series_id=s.id "
            "LEFT JOIN st.progress p ON p.id=i.id WHERE s.library=? GROUP BY s.id "
            "ORDER BY s.sort_name", (name,))
        movies = self.db.query(
            self.LIST_SQL + " WHERE i.library=? AND i.kind='movie' ORDER BY i.sort_name", (name,))
        return {
            "name": name,
            "series": [dict(r) for r in series],
            "movies": [self.item_json(r) for r in movies],
        }

    def series(self, sid: str) -> dict:
        s = self.db.one("SELECT * FROM series WHERE id=?", (sid,))
        if s is None:
            raise HttpError(404, "no such series")
        eps = self.episodes(sid)
        resume = next((e for e in eps if self.resume_point(e) > 0), None)
        if resume is None:
            regular = [e for e in eps if e["season"] != 0]
            last = max((n for n, e in enumerate(regular) if e["watched"]), default=-1)
            resume = next((e for e in regular[last + 1:] if not e["watched"]), None)
            if resume is None and regular and last == -1:
                resume = regular[0]
        return {
            "id": s["id"], "title": s["title"], "year": s["year"], "library": s["library"],
            "episodes": [self.item_json(e) for e in eps],
            "next_id": resume["id"] if resume else None,
        }

    def search(self, q: str) -> dict:
        q = q.strip()
        if not q:
            return {"series": [], "movies": []}
        like = "%" + q.replace("%", r"\%").replace("_", r"\_") + "%"
        series = self.db.query(
            "SELECT id, title, year, library FROM series WHERE title LIKE ? ESCAPE '\\' "
            "ORDER BY sort_name LIMIT 50", (like,))
        movies = self.db.query(
            self.LIST_SQL + " WHERE i.kind='movie' AND (i.title LIKE ? ESCAPE '\\' "
            "OR i.rel_path LIKE ? ESCAPE '\\') ORDER BY i.sort_name LIMIT 50", (like, like))
        return {"series": [dict(r) for r in series], "movies": [self.item_json(r) for r in movies]}

    def playback_info(self, iid: str, query: dict) -> dict:
        r = self.ensure_probed(self.get_item(iid))
        item = dict(r)
        item["streams"] = json.loads(r["streams"] or "[]")
        caps = playback.Caps.parse(_q(query, "vcaps"), _q(query, "acaps"))
        audio = _q(query, "audio")
        audio_index = int(audio) if audio and audio.isdigit() else None
        force = _q(query, "force")
        d = playback.decide(item, caps, self.cfg.audio_languages, audio_index, force)

        t_raw = _q(query, "t")
        start = float(t_raw) if t_raw not in (None, "") else self.resume_point(r)
        if r["duration"]:
            start = min(max(0.0, start), max(0.0, r["duration"] - 1))
        if d.mode == "direct":
            url = f"/file/{iid}"
        else:
            if start > 0 and d.video == "copy":
                kf = probe.keyframe_before(self.cfg.ffprobe, r["path"], start,
                                           r["start_time"] or 0.0)
                if kf is not None and start - kf < 30:
                    start = kf
            params = {"t": f"{start:.3f}", "vcaps": _q(query, "vcaps") or "",
                      "acaps": _q(query, "acaps") or "", "sid": _q(query, "sid") or "",
                      "force": force or ""}
            if d.audio_index is not None:
                params["audio"] = str(d.audio_index)
            url = f"/stream/{iid}?" + urllib.parse.urlencode(params)
        prev_id, next_id = self.neighbours(r)
        return {
            "item": self.item_json(r),
            "mode": d.mode, "reason": d.reason, "video": d.video, "audio": d.audio,
            "url": url, "start": start, "duration": r["duration"],
            "audio_tracks": playback.describe_audio(item["streams"]),
            "audio_index": d.audio_index, "prev_id": prev_id, "next_id": next_id,
        }

    # ---- streams ---------------------------------------------------------
    def register_stream(self, sid: str, proc: subprocess.Popen) -> None:
        if not sid:
            return
        with self.streams_lock:
            old = self.streams.get(sid)
            self.streams[sid] = proc
        if old is not None and old.poll() is None:
            old.kill()

    def unregister_stream(self, sid: str, proc: subprocess.Popen) -> None:
        with self.streams_lock:
            if self.streams.get(sid) is proc:
                del self.streams[sid]

    def stop_stream(self, sid: str) -> None:
        with self.streams_lock:
            proc = self.streams.pop(sid, None)
        if proc is not None and proc.poll() is None:
            proc.kill()


def _q(query: dict, name: str) -> str | None:
    v = query.get(name)
    return v[0] if v else None


ROUTES: list[tuple[str, re.Pattern, str, bool]] = []


def route(method: str, pattern: str, auth: bool = True):
    def deco(fn):
        ROUTES.append((method, re.compile(f"^{pattern}$"), fn.__name__, auth))
        return fn
    return deco


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "mediawatcher"
    app: App  # set on the subclass created by make_server

    def log_message(self, fmt, *args):
        log.debug("%s %s", self.address_string(), fmt % args)

    # ---- plumbing --------------------------------------------------------
    def _dispatch(self, method: str):
        split = urllib.parse.urlsplit(self.path)
        self.query = urllib.parse.parse_qs(split.query)
        path = urllib.parse.unquote(split.path)
        self._body = b""
        try:
            # Always consume the body first, so an early error response cannot
            # leave unread bytes on a keep-alive connection.
            n = int(self.headers.get("Content-Length") or 0)
            if n > 1_000_000:
                self.close_connection = True
                raise HttpError(413)
            if n:
                self._body = self.rfile.read(n)
            for m, pat, name, auth in ROUTES:
                if m != method and not (method == "HEAD" and m == "GET"):
                    continue
                match = pat.match(path)
                if match:
                    if auth and not self.app.authorized(self.headers, self.query, self.cookies()):
                        raise HttpError(401, "unauthorized")
                    getattr(self, name)(*match.groups())
                    return
            raise HttpError(404, "not found")
        except HttpError as e:
            self.send_json({"error": e.message}, e.status)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception:  # noqa: BLE001
            log.exception("error handling %s %s", method, self.path)
            try:
                self.send_json({"error": "internal error"}, 500)
            except OSError:
                pass

    def do_GET(self):
        self._dispatch("GET")

    def do_HEAD(self):
        self._dispatch("HEAD")

    def do_POST(self):
        self._dispatch("POST")

    def cookies(self) -> dict:
        out = {}
        for part in (self.headers.get("Cookie") or "").split(";"):
            if "=" in part:
                k, v = part.strip().split("=", 1)
                out[k] = v
        return out

    def body_json(self) -> dict:
        raw = self._body
        if not raw:
            return {}
        try:
            data = json.loads(raw)
        except ValueError:
            raise HttpError(400, "invalid json") from None
        if not isinstance(data, dict):
            raise HttpError(400, "expected an object")
        return data

    def send_bytes(self, body: bytes, ctype: str, status: int = 200, headers: dict | None = None):
        if len(body) > 1024 and "gzip" in (self.headers.get("Accept-Encoding") or "") and \
                (ctype.startswith("application/json") or ctype.startswith("text/")
                 or ctype.startswith("application/javascript")):
            body = gzip.compress(body, compresslevel=5)
            headers = {**(headers or {}), "Content-Encoding": "gzip", "Vary": "Accept-Encoding"}
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def send_json(self, data, status: int = 200, headers: dict | None = None):
        body = json.dumps(data, separators=(",", ":")).encode()
        self.send_bytes(body, "application/json; charset=utf-8", status,
                        {"Cache-Control": "no-store", **(headers or {})})

    def send_static_file(self, path: str, ctype: str, cache: str):
        try:
            st = os.stat(path)
        except OSError:
            raise HttpError(404) from None
        etag = f'"{int(st.st_mtime)}-{st.st_size}"'
        if self.headers.get("If-None-Match") == etag:
            self.send_response(304)
            self.send_header("ETag", etag)
            self.send_header("Cache-Control", cache)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        with open(path, "rb") as f:
            body = f.read()
        self.send_bytes(body, ctype, 200, {"ETag": etag, "Cache-Control": cache})

    # ---- web UI ----------------------------------------------------------
    @route("GET", r"/healthz", auth=False)
    def healthz(self):
        self.send_json({"ok": True})

    @route("GET", r"/(|index\.html|app\.js|style\.css|favicon\.svg)", auth=False)
    def static(self, name):
        name = name or "index.html"
        ctype = {"html": "text/html; charset=utf-8", "js": "application/javascript; charset=utf-8",
                 "css": "text/css; charset=utf-8", "svg": "image/svg+xml"}[name.rsplit(".", 1)[1]]
        self.send_static_file(os.path.join(self.app.cfg.web_dir, name), ctype, "no-cache")

    # ---- auth ------------------------------------------------------------
    @route("POST", r"/api/login", auth=False)
    def login(self):
        key = str(self.body_json().get("key") or "")
        if not hmac.compare_digest(key.encode(), self.app.cfg.api_key.encode()):
            time.sleep(1.0)  # slow down guessing
            raise HttpError(401, "wrong key")
        cookie = (f"{COOKIE}={self.app.session_token}; Path=/; HttpOnly; SameSite=Strict; "
                  f"Max-Age={400 * 24 * 3600}")
        self.send_json({"ok": True}, headers={"Set-Cookie": cookie})

    @route("POST", r"/api/logout", auth=False)
    def logout(self):
        self.send_json({"ok": True},
                       headers={"Set-Cookie": f"{COOKIE}=; Path=/; HttpOnly; Max-Age=0"})

    # ---- browsing --------------------------------------------------------
    @route("GET", r"/api/home")
    def api_home(self):
        self.send_json({**self.app.home(), "status": self.app.scanner.status})

    @route("GET", r"/api/libraries/([^/]+)")
    def api_library(self, name):
        self.send_json(self.app.library(name))

    @route("GET", r"/api/series/([0-9a-f]+)")
    def api_series(self, sid):
        self.send_json(self.app.series(sid))

    @route("GET", r"/api/items/([0-9a-f]+)")
    def api_item(self, iid):
        r = self.app.get_item(iid)
        out = self.app.item_json(r)
        out["streams"] = json.loads(r["streams"] or "[]")
        out["path"] = r["rel_path"]
        self.send_json(out)

    @route("GET", r"/api/search")
    def api_search(self):
        self.send_json(self.app.search(_q(self.query, "q") or ""))

    @route("GET", r"/api/status")
    def api_status(self):
        self.send_json(self.app.scanner.status)

    @route("POST", r"/api/scan")
    def api_scan(self):
        self.app.scanner.trigger()
        self.send_json({"ok": True})

    # ---- progress --------------------------------------------------------
    @route("POST", r"/api/items/([0-9a-f]+)/progress")
    def api_progress(self, iid):
        b = self.body_json()
        try:
            pos = float(b.get("position") or 0)
            dur = float(b["duration"]) if b.get("duration") else None
        except (TypeError, ValueError):
            raise HttpError(400, "position must be a number") from None
        self.send_json(self.app.save_progress(iid, pos, dur, bool(b.get("ended"))))

    @route("POST", r"/api/items/([0-9a-f]+)/watched")
    def api_item_watched(self, iid):
        self.app.get_item(iid)
        self.app.set_watched([iid], bool(self.body_json().get("watched", True)))
        self.send_json({"ok": True})

    @route("POST", r"/api/series/([0-9a-f]+)/watched")
    def api_series_watched(self, sid):
        b = self.body_json()
        ids = [e["id"] for e in self.app.episode_order(sid)
               if b.get("season") is None or e["season"] == b.get("season")]
        self.app.set_watched(ids, bool(b.get("watched", True)))
        self.send_json({"ok": True})

    # ---- images ----------------------------------------------------------
    @route("GET", r"/img/(item|series)/([0-9a-f]+)")
    def image(self, kind, id_):
        # Grid pages fire dozens of these at once: serve a cached image straight
        # from disk, without touching SQLite. The scanner drops stale ones.
        try:
            self.send_static_file(self.app.images.cache_path(kind, id_), "image/jpeg",
                                  "private, max-age=86400")
            return
        except HttpError:
            pass  # not cached yet: look it up and generate it
        path = self.app.images.item_image(id_) if kind == "item" else self.app.images.series_image(id_)
        if not path:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.send_header("Cache-Control", "private, max-age=300")
            self.end_headers()
            return
        self.send_static_file(path, "image/jpeg", "private, max-age=86400")

    # ---- playback --------------------------------------------------------
    @route("GET", r"/api/items/([0-9a-f]+)/playback")
    def api_playback(self, iid):
        self.send_json(self.app.playback_info(iid, self.query))

    @route("POST", r"/api/streams/([A-Za-z0-9_-]+)/stop")
    def api_stop_stream(self, sid):
        self.app.stop_stream(sid)
        self.send_json({"ok": True})

    @route("GET", r"/file/([0-9a-f]+)")
    def file(self, iid):
        """The original file with byte-range support: direct play, downloads, VLC/mpv."""
        r = self.app.get_item(iid)
        path = r["path"]
        try:
            f = open(path, "rb")
        except OSError:
            raise HttpError(404, "file not readable") from None
        with f:
            size = os.fstat(f.fileno()).st_size
            start, end = 0, size - 1
            status = 200
            rng = self.headers.get("Range")
            if rng:
                m = re.match(r"bytes=(\d*)-(\d*)$", rng.strip())
                if not m or (not m.group(1) and not m.group(2)):
                    raise HttpError(416)
                if m.group(1):
                    start = int(m.group(1))
                    if m.group(2):
                        end = min(int(m.group(2)), size - 1)
                else:  # suffix range: last N bytes
                    start = max(0, size - int(m.group(2)))
                if start >= size or start > end:
                    self.send_response(416)
                    self.send_header("Content-Range", f"bytes */{size}")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                status = 206
            ext = os.path.splitext(path)[1].lower()
            self.send_response(status)
            self.send_header("Content-Type", FILE_TYPES.get(ext) or mimetypes.guess_type(path)[0]
                             or "application/octet-stream")
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Content-Length", str(end - start + 1))
            self.send_header("Last-Modified", email.utils.formatdate(r["mtime"], usegmt=True))
            self.send_header("Cache-Control", "private, max-age=3600")
            if status == 206:
                self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
            if _q(self.query, "download"):
                name = urllib.parse.quote(os.path.basename(path))
                self.send_header("Content-Disposition", f"attachment; filename*=UTF-8''{name}")
            self.end_headers()
            if self.command == "HEAD":
                return
            self.connection.sendfile(f, start, end - start + 1)

    @route("GET", r"/stream/([0-9a-f]+)")
    def stream(self, iid):
        """Remux/transcode to fragmented MP4 on the fly, starting at ?t=."""
        app = self.app
        rng = self.headers.get("Range")
        if rng and not re.match(r"bytes=0-\s*$", rng.strip()):
            # A live stream cannot jump to a byte offset; the player seeks by time.
            self.send_response(416)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        r = app.ensure_probed(app.get_item(iid))
        item = dict(r)
        item["streams"] = json.loads(r["streams"] or "[]")
        caps = playback.Caps.parse(_q(self.query, "vcaps"), _q(self.query, "acaps"))
        audio = _q(self.query, "audio")
        d = playback.decide(item, caps, app.cfg.audio_languages,
                            int(audio) if audio and audio.isdigit() else None,
                            _q(self.query, "force") or "remux")
        try:
            start = max(0.0, float(_q(self.query, "t") or 0))
        except ValueError:
            raise HttpError(400, "bad t") from None
        args = playback.ffmpeg_args(app.cfg, item, d, start)

        self.send_response(200)
        self.send_header("Content-Type", "video/mp4")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        if self.command == "HEAD":
            return

        log.info("stream %s (%s, video=%s audio=%s) from %.1fs", r["rel_path"], d.mode,
                 d.video, d.audio, start)
        proc = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, bufsize=0)
        errors: collections.deque[bytes] = collections.deque(maxlen=20)

        def drain():
            for line in proc.stderr:
                errors.append(line)

        threading.Thread(target=drain, daemon=True).start()
        sid = _q(self.query, "sid") or ""
        app.register_stream(sid, proc)
        sent = 0
        try:
            while True:
                chunk = proc.stdout.read(CHUNK)
                if not chunk:
                    break
                self.wfile.write(chunk)
                sent += len(chunk)
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass  # player went away (seek, close, next episode)
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait()
            app.unregister_stream(sid, proc)
            if proc.returncode not in (0, -9) and errors:
                log.warning("ffmpeg exited %s for %s: %s", proc.returncode, r["rel_path"],
                            b"".join(errors).decode(errors="replace").strip())
            log.debug("stream %s closed after %d bytes", iid, sent)


class Server(ThreadingHTTPServer):
    daemon_threads = True
    request_queue_size = 64
    allow_reuse_address = True


def make_server(app: App) -> Server:
    handler = type("BoundHandler", (Handler,), {"app": app})
    return Server((app.cfg.host, app.cfg.port), handler)
