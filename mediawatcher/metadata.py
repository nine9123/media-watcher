"""Optional metadata from TMDB: posters and overviews for series and movies.

Off unless TMDB_API_KEY is set. Runs as one phase of every scan and only asks
TMDB about titles it has not looked up yet, whose folder name changed, or
whose last lookup is older than METADATA_REFRESH_DAYS, so a scan with nothing
new makes no requests at all.

A poster next to the media (poster.jpg, folder.jpg, ...) always wins over the
downloaded one. A wrong match is fixed by putting the TMDB id in the folder or
file name, Jellyfin style: "Show (2020) [tmdbid-12345]" or "{tmdb-12345}".
Everything is stored in the disposable library.db and CACHE_DIR, so wiping the
cache only costs one more round of lookups.
"""

from __future__ import annotations

import json
import logging
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request

from .config import Config
from .db import Database
from .images import Images

log = logging.getLogger("mediawatcher.metadata")

API = "https://api.themoviedb.org/3"
IMAGE_BASE = "https://image.tmdb.org/t/p/w500"
USER_AGENT = "mediawatcher"
# TMDB allows roughly 40 requests a second; one title costs one to three.
PAUSE = 0.1

_ID_TAG = re.compile(r"[\[{]tmdb(?:id)?[-=: ]?(\d+)[\]}]", re.I)


def tmdb_id_from_path(path: str | None) -> int | None:
    """`[tmdbid-123]` or `{tmdb-123}` anywhere in a path."""
    m = _ID_TAG.search(path or "")
    return int(m.group(1)) if m else None


def _year(date: str | None) -> int | None:
    return int(date[:4]) if date and date[:4].isdigit() else None


class TmdbError(Exception):
    """TMDB could not be reached or refused us; try again on the next scan."""


class Tmdb:
    """Just the three calls the scanner needs. `fetch(url, headers) -> bytes`
    can be swapped out in tests."""

    def __init__(self, key: str, language: str = "en-US", fetch=None):
        self.key = key
        self.language = language
        self.fetch = fetch or self._urlopen

    @staticmethod
    def _urlopen(url: str, headers: dict) -> bytes:
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=20) as r:
            return r.read()

    def _get(self, path: str, **params) -> dict | None:
        """JSON from the API; None for 404."""
        headers = {"Accept": "application/json", "User-Agent": USER_AGENT}
        if self.key.startswith("eyJ"):  # v4 read access token
            headers["Authorization"] = f"Bearer {self.key}"
        else:  # v3 API key
            params["api_key"] = self.key
        params["language"] = self.language
        query = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None})
        try:
            return json.loads(self.fetch(f"{API}{path}?{query}", headers))
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return None
            raise TmdbError(f"HTTP {e.code} for {path}") from None
        except (OSError, ValueError) as e:  # URLError, timeouts, bad JSON
            raise TmdbError(f"{path}: {e}") from None

    def find(self, kind: str, title: str, year: int | None, tmdb_id: int | None) -> dict | None:
        """The best match for a series (`kind="tv"`) or movie, or None.

        Returns {tmdb_id, title, year, overview, poster} with poster being
        TMDB's image path ("/abc.jpg") or None.
        """
        if tmdb_id:
            hit = self._get(f"/{kind}/{tmdb_id}")
        else:
            hit = None
            year_param = "first_air_date_year" if kind == "tv" else "year"
            # With the year first (remakes), then without (folders with a wrong year).
            for y in ([year, None] if year else [None]):
                res = self._get(f"/search/{kind}", query=title, include_adult="false",
                                **{year_param: y})
                results = (res or {}).get("results") or []
                if results:
                    hit = results[0]
                    break
        if not hit or not hit.get("id"):
            return None
        return {
            "tmdb_id": hit["id"],
            "title": hit.get("name") or hit.get("title"),
            "year": _year(hit.get("first_air_date") or hit.get("release_date")),
            "overview": hit.get("overview") or None,
            "poster": hit.get("poster_path") or None,
        }

    def image(self, poster: str) -> bytes:
        try:
            return self.fetch(IMAGE_BASE + poster, {"User-Agent": USER_AGENT})
        except OSError as e:
            raise TmdbError(f"poster {poster}: {e}") from None


class Metadata:
    def __init__(self, cfg: Config, db: Database, images: Images, client: Tmdb | None = None):
        self.cfg = cfg
        self.db = db
        self.images = images
        self.client = client or Tmdb(cfg.tmdb_api_key, cfg.tmdb_language)

    def targets(self) -> list[tuple[str, str, str, int | None, int | None]]:
        """(kind, id, title, year, tmdb id from the path) for every series and movie."""
        out = []
        for r in self.db.query("SELECT id, title, year, dir FROM series"):
            out.append(("series", r["id"], r["title"], r["year"], tmdb_id_from_path(r["dir"])))
        for r in self.db.query("SELECT id, title, year, rel_path FROM items WHERE kind='movie'"):
            out.append(("item", r["id"], r["title"], r["year"], tmdb_id_from_path(r["rel_path"])))
        return out

    def refresh(self, status: dict | None = None) -> None:
        targets = self.targets()
        self._drop_orphans({t[1] for t in targets})
        known = {r["id"]: r for r in self.db.query(
            "SELECT id, query, poster_url, fetched_at FROM metadata")}
        cutoff = time.time() - self.cfg.metadata_refresh_days * 86400
        todo = []
        for kind, id_, title, year, tmdb_id in targets:
            query = f"{title}|{year or ''}|{tmdb_id or ''}"
            old = known.get(id_)
            if old is None or old["query"] != query or old["fetched_at"] < cutoff:
                todo.append((kind, id_, title, year, tmdb_id, query, old))
        if status is not None:
            status.update(to_fetch=len(todo), fetched=0)
        for kind, id_, title, year, tmdb_id, query, old in todo:
            try:
                self.update_one(kind, id_, title, year, tmdb_id, query, old)
            except TmdbError as e:
                # Offline, bad key, rate limited: leave everything as it is.
                log.warning("metadata lookup stopped: %s", e)
                if status is not None:
                    status["error"] = f"metadata: {e}"
                return
            if status is not None:
                status["fetched"] += 1
            time.sleep(PAUSE)

    def update_one(self, kind, id_, title, year, tmdb_id, query, old) -> None:
        found = self.client.find("tv" if kind == "series" else "movie", title, year, tmdb_id)
        poster_url = found["poster"] if found else None
        path = self.images.meta_poster_path(id_)
        if poster_url and (not old or old["poster_url"] != poster_url or not os.path.exists(path)):
            data = self.client.image(poster_url)
            tmp = path + ".tmp"
            with open(tmp, "wb") as f:
                f.write(data)
            os.replace(tmp, path)
            self.images.remove(kind, id_)  # served from disk without a lookup: drop it now
        elif not poster_url and os.path.exists(path):
            os.remove(path)
            self.images.remove(kind, id_)
        now = time.time()
        # art_at versions the image URL: only a different poster changes it.
        changed = (old["poster_url"] if old else None) != poster_url
        self.db.write(
            "INSERT INTO metadata(id, kind, query, tmdb_id, title, year, overview, poster_url, "
            "art_at, fetched_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET "
            "kind=excluded.kind, query=excluded.query, tmdb_id=excluded.tmdb_id, "
            "title=excluded.title, year=excluded.year, overview=excluded.overview, "
            "poster_url=excluded.poster_url, fetched_at=excluded.fetched_at"
            + (", art_at=excluded.art_at" if changed else ""),
            (id_, kind, query, found and found["tmdb_id"], found and found["title"],
             found and found["year"], found and found["overview"], poster_url,
             now if changed else 0, now),
        )
        if found:
            log.info("metadata %s %r -> tmdb %s %r", kind, title, found["tmdb_id"], found["title"])
        else:
            log.info("metadata %s %r: no match", kind, title)

    def _drop_orphans(self, live: set[str]) -> None:
        gone = [r["id"] for r in self.db.query("SELECT id FROM metadata") if r["id"] not in live]
        if gone:
            self.db.write_many([("DELETE FROM metadata WHERE id=?", (i,)) for i in gone])
            for i in gone:
                try:
                    os.remove(self.images.meta_poster_path(i))
                except FileNotFoundError:
                    pass
