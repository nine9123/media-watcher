"""Metadata from TMDB and AniList: posters and overviews for series and movies.

Sources are tried in METADATA_SOURCES order (default TMDB, then AniList) and
the first match wins. TMDB needs an API key; AniList needs none, so metadata
is on unless METADATA_SOURCES is emptied. Runs as one phase of every scan and
only looks up titles it has not looked up yet, whose folder name changed, or
whose last lookup is older than METADATA_REFRESH_DAYS, so a scan with nothing
new makes no requests at all.

A poster next to the media (poster.jpg, folder.jpg, ...) always wins over the
downloaded one. A wrong match is fixed by putting the id in the folder or
file name, Jellyfin style: "Show (2020) [tmdbid-12345]", "{tmdb-12345}" or
"[anilistid-12345]". An id tag skips the search and the other sources.
Everything is stored in the disposable library.db and CACHE_DIR, so wiping the
cache only costs one more round of lookups.
"""

from __future__ import annotations

import html
import json
import logging
import os
import re
import time
import urllib.error
import urllib.parse
import unicodedata
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
ANILIST_API = "https://graphql.anilist.co"
# AniList allows 90 requests a minute (30 while it is degraded); one title is one.
ANILIST_PAUSE = 2.1

_ID_TAG = re.compile(r"[\[{]tmdb(?:id)?[-=: ]?(\d+)[\]}]", re.I)
_ANILIST_TAG = re.compile(r"[\[{]anilist(?:id)?[-=: ]?(\d+)[\]}]", re.I)


def tmdb_id_from_path(path: str | None) -> int | None:
    """`[tmdbid-123]` or `{tmdb-123}` anywhere in a path."""
    m = _ID_TAG.search(path or "")
    return int(m.group(1)) if m else None


def anilist_id_from_path(path: str | None) -> int | None:
    """`[anilistid-123]` or `{anilist-123}` anywhere in a path."""
    m = _ANILIST_TAG.search(path or "")
    return int(m.group(1)) if m else None


def _year(date: str | None) -> int | None:
    return int(date[:4]) if date and date[:4].isdigit() else None


class MetadataError(Exception):
    """A source could not be reached or refused us; try again on the next scan."""


class TmdbError(MetadataError):
    pass


class AnilistError(MetadataError):
    pass


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
            "source": "tmdb",
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


def _words(s: str | None) -> str:
    """Lowercase alphanumeric words: "Frieren: Beyond…" -> "frieren beyond"."""
    s = unicodedata.normalize("NFKC", s or "").lower()
    return " ".join(re.findall(r"\w+", s))


def _plain(description: str | None) -> str | None:
    """AniList descriptions carry <br>, <i> and HTML entities."""
    if not description:
        return None
    s = re.sub(r"<br\s*/?>", "\n", description, flags=re.I)
    s = html.unescape(re.sub(r"<[^>]+>", "", s))
    s = re.sub(r"\n{3,}", "\n\n", s).strip()
    return s or None


_SUBTITLE = re.compile(r"\s*(?::|\s[-–—~]\s|\()")

_ANILIST_FIELDS = """id format synonyms title { romaji english native } startDate { year }
  description(asHtml: false) coverImage { extraLarge large }"""
_ANILIST_SEARCH = """query ($search: String, $formats: [MediaFormat]) {
  Page(perPage: 10) { media(search: $search, type: ANIME, format_in: $formats,
    sort: SEARCH_MATCH, isAdult: false) { %s } } }""" % _ANILIST_FIELDS
_ANILIST_BY_ID = "query ($id: Int) { Media(id: $id, type: ANIME) { %s } }" % _ANILIST_FIELDS
_ANILIST_FORMATS = {
    "tv": ["TV", "TV_SHORT", "ONA", "OVA", "SPECIAL"],
    "movie": ["MOVIE", "OVA", "ONA", "SPECIAL"],
}


class Anilist:
    """Anime only, no key needed. `fetch(url, headers, body) -> bytes` can be
    swapped out in tests.

    AniList's search is fuzzy and will happily turn "Heat" into some anime,
    so a result only counts when one of its titles or synonyms is the folder
    title, with or without a subtitle ("Frieren" -> "Frieren: Beyond Journey's
    End", but not "Heat" -> "Heat Haze Days"). Among those, see rank() below.
    """

    def __init__(self, fetch=None):
        self.fetch = fetch or self._urlopen

    @staticmethod
    def _urlopen(url: str, headers: dict, body: bytes | None = None) -> bytes:
        req = urllib.request.Request(url, data=body, headers=headers)
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.read()

    def _post(self, query: str, **variables) -> dict:
        headers = {"Accept": "application/json", "Content-Type": "application/json",
                   "User-Agent": USER_AGENT}
        body = json.dumps({"query": query, "variables": variables}).encode()
        try:
            res = json.loads(self.fetch(ANILIST_API, headers, body))
        except urllib.error.HTTPError as e:
            if e.code == 404:  # Media(id:) that does not exist
                return {}
            raise AnilistError(f"HTTP {e.code}") from None
        except (OSError, ValueError) as e:
            raise AnilistError(str(e)) from None
        if res.get("errors") and not res.get("data"):
            raise AnilistError(str(res["errors"][0].get("message", res["errors"])))
        return res.get("data") or {}

    @staticmethod
    def _matches(media: dict, title: str) -> int:
        """2 when one of the titles matches, 1 for only a synonym, else 0."""
        want = _words(title)
        if not want:
            return 0

        def hit(names):
            # The whole title, or its main part before a subtitle.
            return any(want in (_words(n), _words(_SUBTITLE.split(n, 1)[0])) for n in names if n)

        if hit((media.get("title") or {}).values()):
            return 2
        return 1 if hit(media.get("synonyms") or []) else 0

    def find(self, kind: str, title: str, year: int | None, anilist_id: int | None) -> dict | None:
        """Same contract as Tmdb.find, with `poster` a full image URL."""
        if anilist_id:
            hit = self._post(_ANILIST_BY_ID, id=anilist_id).get("Media")
        else:
            formats = _ANILIST_FORMATS[kind]
            page = self._post(_ANILIST_SEARCH, search=title, formats=formats)
            media = (page.get("Page") or {}).get("media") or []

            def rank(m):
                # Year (±1), then a real title over a synonym (commercials and
                # specials borrow the film's name), then the main format.
                y = (m.get("startDate") or {}).get("year")
                near = year is not None and y is not None and abs(y - year) <= 1
                return (near, self._matches(m, title), m.get("format") == formats[0])

            # max() keeps the first of equals: AniList's own search order.
            found = [m for m in media if self._matches(m, title)]
            hit = max(found, key=rank) if found else None
        if not hit or not hit.get("id"):
            return None
        names = hit.get("title") or {}
        cover = hit.get("coverImage") or {}
        return {
            "source": "anilist",
            "anilist_id": hit["id"],
            "title": names.get("english") or names.get("romaji") or names.get("native"),
            "year": (hit.get("startDate") or {}).get("year"),
            "overview": _plain(hit.get("description")),
            "poster": cover.get("extraLarge") or cover.get("large") or None,
        }

    def image(self, poster: str) -> bytes:
        try:
            return self.fetch(poster, {"User-Agent": USER_AGENT}, None)
        except OSError as e:
            raise AnilistError(f"poster {poster}: {e}") from None


def sources(cfg: Config) -> list[str]:
    """The configured sources that can actually be used, in order."""
    return [s for s in cfg.metadata_sources if s == "anilist" or (s == "tmdb" and cfg.tmdb_api_key)]


class Metadata:
    def __init__(self, cfg: Config, db: Database, images: Images, client: Tmdb | None = None,
                 anilist: Anilist | None = None):
        self.cfg = cfg
        self.db = db
        self.images = images
        self.sources = sources(cfg)
        self.clients = {
            "tmdb": client or Tmdb(cfg.tmdb_api_key, cfg.tmdb_language),
            "anilist": anilist or Anilist(),
        }

    def targets(self) -> list[tuple[str, str, str, int | None, tuple[int | None, int | None]]]:
        """(kind, id, title, year, (tmdb id, anilist id) from the path) for every
        series and movie."""
        out = []
        for r in self.db.query("SELECT id, title, year, dir FROM series"):
            out.append(("series", r["id"], r["title"], r["year"], self._tags(r["dir"])))
        for r in self.db.query("SELECT id, title, year, rel_path FROM items WHERE kind='movie'"):
            out.append(("item", r["id"], r["title"], r["year"], self._tags(r["rel_path"])))
        return out

    @staticmethod
    def _tags(path: str | None) -> tuple[int | None, int | None]:
        return tmdb_id_from_path(path), anilist_id_from_path(path)

    def find(self, kind: str, title: str, year: int | None,
             tags: tuple[int | None, int | None]) -> dict | None:
        """First match over the sources; an id tag pins its own source."""
        tmdb_id, anilist_id = tags
        kind = "tv" if kind == "series" else "movie"
        if anilist_id and "anilist" in self.sources:
            return self._find("anilist", kind, title, year, anilist_id)
        if tmdb_id and "tmdb" in self.sources:
            return self._find("tmdb", kind, title, year, tmdb_id)
        for source in self.sources:
            found = self._find(source, kind, title, year, None)
            if found:
                return found
        return None

    def _find(self, source, kind, title, year, id_) -> dict | None:
        found = self.clients[source].find(kind, title, year, id_)
        time.sleep(PAUSE if source == "tmdb" else ANILIST_PAUSE)
        return found

    def refresh(self, status: dict | None = None) -> None:
        targets = self.targets()
        self._drop_orphans({t[1] for t in targets})
        known = {r["id"]: r for r in self.db.query(
            "SELECT id, query, poster_url, fetched_at FROM metadata")}
        cutoff = time.time() - self.cfg.metadata_refresh_days * 86400
        todo = []
        # The sources are part of the key: adding a TMDB key or a source
        # looks everything up again, including earlier misses.
        used = "+".join(self.sources)
        for kind, id_, title, year, tags in targets:
            query = f"{title}|{year or ''}|{tags[0] or ''}|{tags[1] or ''}|{used}"
            old = known.get(id_)
            if old is None or old["query"] != query or old["fetched_at"] < cutoff:
                todo.append((kind, id_, title, year, tags, query, old))
        if status is not None:
            status.update(to_fetch=len(todo), fetched=0)
        for kind, id_, title, year, tags, query, old in todo:
            try:
                self.update_one(kind, id_, title, year, tags, query, old)
            except MetadataError as e:
                # Offline, bad key, rate limited: leave everything as it is.
                log.warning("metadata lookup stopped: %s", e)
                if status is not None:
                    status["error"] = f"metadata: {e}"
                return
            if status is not None:
                status["fetched"] += 1

    def update_one(self, kind, id_, title, year, tags, query, old) -> None:
        found = self.find(kind, title, year, tags)
        poster_url = found["poster"] if found else None
        path = self.images.meta_poster_path(id_)
        if poster_url and (not old or old["poster_url"] != poster_url or not os.path.exists(path)):
            data = self.clients[found["source"]].image(poster_url)
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
            "INSERT INTO metadata(id, kind, query, tmdb_id, anilist_id, title, year, overview, "
            "poster_url, art_at, fetched_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) "
            "DO UPDATE SET kind=excluded.kind, query=excluded.query, tmdb_id=excluded.tmdb_id, "
            "anilist_id=excluded.anilist_id, title=excluded.title, year=excluded.year, "
            "overview=excluded.overview, poster_url=excluded.poster_url, "
            "fetched_at=excluded.fetched_at" + (", art_at=excluded.art_at" if changed else ""),
            (id_, kind, query, found and found.get("tmdb_id"), found and found.get("anilist_id"),
             found and found["title"], found and found["year"], found and found["overview"],
             poster_url, now if changed else 0, now),
        )
        if found:
            log.info("metadata %s %r -> %s %s %r", kind, title, found["source"],
                     found.get("tmdb_id") or found.get("anilist_id"), found["title"])
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
