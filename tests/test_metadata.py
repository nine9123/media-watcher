"""TMDB and AniList metadata against fake APIs: matching, poster download, refresh rules."""

import json
import os
import shutil
import subprocess
import urllib.error
import urllib.parse

import pytest

from mediawatcher import metadata
from mediawatcher import config
from mediawatcher.config import Config, Library
from mediawatcher.db import Database
from mediawatcher.images import Images
from mediawatcher.metadata import (Anilist, AnilistError, Metadata, Tmdb, TmdbError,
                                   anilist_id_from_path, sources, tmdb_id_from_path)
from mediawatcher.scanner import Scanner
from mediawatcher.server import App

POSTER = b"\xff\xd8fake-jpeg"


def touch(root, rel):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(b"\0" * 2048)


class FakeTmdb:
    """Answers like api.themoviedb.org for a couple of titles and logs every request."""

    def __init__(self):
        self.calls = []
        self.down = False
        self.poster = POSTER
        self.tv = {"Show": {"id": 100, "name": "The Show", "first_air_date": "2020-04-01",
                            "overview": "A show.", "poster_path": "/show.jpg"}}
        self.movies = {"Heat": {"id": 949, "title": "Heat", "release_date": "1995-12-15",
                                "overview": "A heist.", "poster_path": "/heat.jpg"}}

    def __call__(self, url, headers):
        self.calls.append(url)
        if self.down:
            raise urllib.error.URLError("network unreachable")
        u = urllib.parse.urlsplit(url)
        if u.netloc == "image.tmdb.org":
            return self.poster
        q = dict(urllib.parse.parse_qsl(u.query))
        assert q["api_key"] == "k" * 32
        path = u.path.removeprefix("/3")
        if path == "/search/tv":
            hit = self.tv.get(q["query"])
            return json.dumps({"results": [hit] if hit else []}).encode()
        if path == "/search/movie":
            hit = self.movies.get(q["query"])
            return json.dumps({"results": [hit] if hit else []}).encode()
        if path == "/tv/555":
            return json.dumps({"id": 555, "name": "Other Show", "overview": "Forced.",
                               "poster_path": "/other.jpg"}).encode()
        raise urllib.error.HTTPError(url, 404, "not found", {}, None)


class FakeAnilist:
    """Answers like graphql.anilist.co, fuzzy search included."""

    def __init__(self):
        self.calls = []
        self.down = False
        self.media = [
            {"id": 154587, "format": "TV", "synonyms": ["Frieren at the Funeral"],
             "title": {"romaji": "Sousou no Frieren", "english": "Frieren: Beyond Journey's End",
                       "native": "葬送のフリーレン"},
             "startDate": {"year": 2023},
             "description": "An elf<br><br>after the <i>journey</i> &amp; more.",
             "coverImage": {"extraLarge": "https://s4.anilist.co/frieren.jpg"}},
            {"id": 777, "format": "MOVIE", "synonyms": [],
             "title": {"romaji": "Heat Haze Days", "english": None, "native": None},
             "startDate": {"year": 2010}, "description": "Not Heat.",
             "coverImage": {"large": "https://s4.anilist.co/haze.jpg"}},
        ]

    def __call__(self, url, headers, body=None):
        self.calls.append((url, json.loads(body) if body else None))
        if self.down:
            raise urllib.error.URLError("network unreachable")
        if url.startswith("https://s4.anilist.co/"):
            return POSTER
        assert url == "https://graphql.anilist.co"
        req = json.loads(body)
        v = req["variables"]
        if "id" in v:
            hit = next((m for m in self.media if m["id"] == v["id"]), None)
            if hit is None:
                raise urllib.error.HTTPError(url, 404, "not found", {}, None)
            return json.dumps({"data": {"Media": hit}}).encode()
        words = v["search"].lower().split()
        found = [m for m in self.media if m["format"] in v["formats"] and any(
            w in json.dumps(m).lower() for w in words)]
        return json.dumps({"data": {"Page": {"media": found}}}).encode()


@pytest.fixture
def env(tmp_path, monkeypatch):
    monkeypatch.setattr(metadata, "PAUSE", 0)
    monkeypatch.setattr(metadata, "ANILIST_PAUSE", 0)
    shows, movies = str(tmp_path / "media" / "Shows"), str(tmp_path / "media" / "Movies")
    touch(shows, "Show (2020)/Season 01/Show - S01E01.mkv")
    touch(shows, "Unknown Thing/Unknown Thing - S01E01.mkv")
    touch(movies, "Heat (1995)/Heat (1995).mkv")
    cfg = Config(
        api_key="secret-key-123",
        libraries=[Library("Shows", shows, "shows"), Library("Movies", movies, "movies")],
        cache_dir=str(tmp_path / "cache"), config_dir=str(tmp_path / "config"),
        ffmpeg="/nonexistent/ffmpeg", ffprobe="/nonexistent/ffprobe", tmdb_api_key="k" * 32,
    )
    db = Database(cfg.cache_dir, cfg.config_dir)
    images = Images(cfg, db)
    fake = FakeTmdb()
    fake_anilist = FakeAnilist()
    meta = Metadata(cfg, db, images, Tmdb(cfg.tmdb_api_key, fetch=fake),
                    Anilist(fetch=fake_anilist))
    scanner = Scanner(cfg, db, images, meta)
    scanner.scan()
    return {"cfg": cfg, "db": db, "images": images, "fake": fake, "anilist": fake_anilist,
            "meta": meta,
            "scanner": scanner, "app": App(cfg, db, images, scanner), "shows": shows}


def ids(env):
    db = env["db"]
    return {
        "show": db.one("SELECT id FROM series WHERE title='Show'")["id"],
        "unknown": db.one("SELECT id FROM series WHERE title='Unknown Thing'")["id"],
        "heat": db.one("SELECT id FROM items WHERE kind='movie'")["id"],
    }


def test_tmdb_id_from_path():
    assert tmdb_id_from_path("Show (2020) [tmdbid-1399]") == 1399
    assert tmdb_id_from_path("Movies/Heat {tmdb-949}/Heat.mkv") == 949
    assert tmdb_id_from_path("Show (2020)") is None
    assert tmdb_id_from_path(None) is None


def test_scan_fetches_posters_and_overviews(env):
    i = ids(env)
    rows = {r["id"]: r for r in env["db"].query("SELECT * FROM metadata")}
    assert rows[i["show"]]["tmdb_id"] == 100
    assert rows[i["heat"]]["overview"] == "A heist."
    assert rows[i["unknown"]]["tmdb_id"] is None  # looked up, no match: remembered
    for key in ("show", "heat"):
        with open(env["images"].meta_poster_path(i[key]), "rb") as f:
            assert f.read() == POSTER
    assert not os.path.exists(env["images"].meta_poster_path(i["unknown"]))
    assert env["scanner"].status["fetched"] == 3

    s = env["app"].series(i["show"])
    assert s["overview"] == "A show." and s["tmdb_id"] == 100 and s["art_v"]
    lib = env["app"].library("Shows")
    assert {x["title"]: bool(x["art_v"]) for x in lib["series"]} == \
        {"Show": True, "Unknown Thing": False}
    assert env["app"].library("Movies")["movies"][0]["art_v"]


def test_second_scan_asks_nothing(env):
    env["fake"].calls.clear()
    env["anilist"].calls.clear()
    env["scanner"].scan()
    assert env["fake"].calls == [] and env["anilist"].calls == []


def test_stale_entries_are_refreshed(env):
    i = ids(env)
    env["db"].write("UPDATE metadata SET fetched_at=0 WHERE id=?", (i["heat"],))
    env["fake"].calls.clear()
    env["scanner"].scan()
    # Same poster as before: one search, no second download.
    assert len(env["fake"].calls) == 1 and "/search/movie" in env["fake"].calls[0]


def test_id_in_folder_name_overrides_search(env):
    os.rename(os.path.join(env["shows"], "Unknown Thing"),
              os.path.join(env["shows"], "Unknown Thing [tmdbid-555]"))
    env["scanner"].scan()
    sid = env["db"].one("SELECT id FROM series WHERE title='Unknown Thing'")["id"]
    s = env["app"].series(sid)
    assert s["tmdb_id"] == 555 and s["overview"] == "Forced."
    assert os.path.exists(env["images"].meta_poster_path(sid))


def test_network_failure_keeps_what_is_there(env):
    i = ids(env)
    env["db"].write("UPDATE metadata SET fetched_at=0")
    env["fake"].down = True
    env["scanner"].scan()
    assert "metadata" in env["scanner"].status["error"]
    assert env["app"].series(i["show"])["overview"] == "A show."
    assert os.path.exists(env["images"].meta_poster_path(i["show"]))
    assert env["db"].one("SELECT MAX(fetched_at) AS t FROM metadata")["t"] == 0


def test_removed_titles_drop_their_metadata(env):
    i = ids(env)
    shutil.rmtree(os.path.join(env["shows"], "Show (2020)"))
    env["scanner"].scan()
    assert env["db"].one("SELECT 1 FROM metadata WHERE id=?", (i["show"],)) is None
    assert not os.path.exists(env["images"].meta_poster_path(i["show"]))


def test_bearer_token_auth():
    seen = {}

    def fetch(url, headers):
        seen.update(url=url, headers=headers)
        return b'{"results": []}'

    Tmdb("eyJhbGciOi.token", fetch=fetch).find("tv", "x", None, None)
    assert seen["headers"]["Authorization"] == "Bearer eyJhbGciOi.token"
    assert "api_key" not in seen["url"]


def test_http_error_is_a_tmdb_error():
    def fetch(url, headers):
        raise urllib.error.HTTPError(url, 401, "unauthorized", {}, None)

    with pytest.raises(TmdbError):
        Tmdb("bad", fetch=fetch).find("movie", "Heat", 1995, None)


@pytest.mark.skipif(not shutil.which("ffmpeg"), reason="needs ffmpeg")
def test_downloaded_poster_replaces_frame_grab(env, tmp_path):
    jpg = tmp_path / "p.jpg"
    subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=red:s=500x750",
                    "-frames:v", "1", "-y", str(jpg)], check=True)
    env["cfg"].ffmpeg = "ffmpeg"
    env["fake"].poster = jpg.read_bytes()
    i = ids(env)
    env["db"].write("UPDATE metadata SET fetched_at=0, poster_url=NULL")
    env["scanner"].scan()
    for kind, id_ in (("series", i["show"]), ("item", i["heat"])):
        out = env["images"].series_image(id_) if kind == "series" else env["images"].item_image(id_)
        assert out == env["images"].cache_path(kind, id_)
        probe = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "stream=width,height",
                                "-of", "csv=p=0", out], capture_output=True, text=True)
        assert probe.stdout.strip() in ("360,540", "480,720")


def test_anilist_id_from_path():
    assert anilist_id_from_path("Frieren [anilistid-154587]") == 154587
    assert anilist_id_from_path("Movies/X {anilist-1}/X.mkv") == 1
    assert anilist_id_from_path("Show [tmdbid-1]") is None


def test_anilist_is_the_fallback_when_tmdb_misses(env):
    touch(env["shows"], "Frieren (2023)/Frieren - S01E01.mkv")
    env["fake"].calls.clear()
    env["anilist"].calls.clear()
    env["scanner"].scan()
    sid = env["db"].one("SELECT id FROM series WHERE title='Frieren'")["id"]
    s = env["app"].series(sid)
    assert s["anilist_id"] == 154587 and s["tmdb_id"] is None
    assert s["overview"] == "An elf\n\nafter the journey & more."
    with open(env["images"].meta_poster_path(sid), "rb") as f:
        assert f.read() == POSTER
    # TMDB was asked first (with and without the year), then AniList once.
    assert all("/search/tv" in c for c in env["fake"].calls) and len(env["fake"].calls) == 2
    assert [u for u, _ in env["anilist"].calls] == \
        ["https://graphql.anilist.co", "https://s4.anilist.co/frieren.jpg"]


def test_tmdb_match_skips_anilist(env):
    # Show and Heat matched on TMDB; only the TMDB miss went to AniList.
    searched = [b["variables"]["search"] for _, b in env["anilist"].calls if b]
    assert searched == ["Unknown Thing"]


def test_anilist_needs_a_matching_title():
    a = Anilist(fetch=FakeAnilist())
    # Fuzzy search finds "Heat Haze Days", which is not "Heat" ...
    assert a.find("movie", "Heat", 1995, None) is None
    # ... but a title that starts with the folder name counts.
    hit = a.find("tv", "Frieren", None, None)
    assert hit["anilist_id"] == 154587 and hit["title"] == "Frieren: Beyond Journey's End"
    assert hit["year"] == 2023 and hit["poster"] == "https://s4.anilist.co/frieren.jpg"
    assert a.find("tv", "Sousou no Frieren", 2023, None)["anilist_id"] == 154587
    assert a.find("tv", "Frieren at the Funeral", None, None)["anilist_id"] == 154587
    assert a.find("tv", "Frieren at the", None, None) is None


def test_anilist_tag_overrides_tmdb(env):
    os.rename(os.path.join(env["shows"], "Show (2020)"),
              os.path.join(env["shows"], "Show (2020) [anilistid-154587]"))
    env["fake"].calls.clear()
    env["scanner"].scan()
    sid = env["db"].one("SELECT id FROM series WHERE title='Show'")["id"]
    s = env["app"].series(sid)
    assert s["anilist_id"] == 154587 and s["tmdb_id"] is None
    assert env["fake"].calls == []


def test_anilist_failure_keeps_what_is_there(env):
    i = ids(env)
    env["db"].write("UPDATE metadata SET fetched_at=0 WHERE id=?", (i["unknown"],))
    env["anilist"].down = True
    env["scanner"].scan()
    assert "metadata" in env["scanner"].status["error"]
    row = env["db"].one("SELECT fetched_at FROM metadata WHERE id=?", (i["unknown"],))
    assert row["fetched_at"] == 0


def test_anilist_errors():
    def fetch(url, headers, body=None):
        raise urllib.error.HTTPError(url, 429, "too many requests", {}, None)

    with pytest.raises(AnilistError):
        Anilist(fetch=fetch).find("tv", "Frieren", None, None)
    assert Anilist(fetch=FakeAnilist()).find("tv", "x", None, 999) is None  # 404 by id


def test_anilist_only_without_tmdb_key(tmp_path, monkeypatch):
    monkeypatch.setattr(metadata, "ANILIST_PAUSE", 0)
    shows = str(tmp_path / "media" / "Shows")
    touch(shows, "Frieren/Frieren - 01.mkv")
    cfg = Config(api_key="secret-key-123", libraries=[Library("Shows", shows, "shows")],
                 cache_dir=str(tmp_path / "cache"), config_dir=str(tmp_path / "config"),
                 ffmpeg="/nonexistent/ffmpeg", ffprobe="/nonexistent/ffprobe")
    assert sources(cfg) == ["anilist"]

    def no_tmdb(url, headers):
        raise AssertionError("TMDB asked without a key")

    db = Database(cfg.cache_dir, cfg.config_dir)
    images = Images(cfg, db)
    meta = Metadata(cfg, db, images, Tmdb("", fetch=no_tmdb), Anilist(fetch=FakeAnilist()))
    Scanner(cfg, db, images, meta).scan()
    assert db.one("SELECT anilist_id FROM metadata")["anilist_id"] == 154587


def test_sources_config():
    base = {"API_KEY": "secret-key-123", "LIBRARIES": ""}
    cfg = config.load(base)
    assert cfg.metadata_sources == ["tmdb", "anilist"] and sources(cfg) == ["anilist"]
    cfg = config.load({**base, "TMDB_DEFAULT_API_KEY": "baked"})
    assert cfg.tmdb_api_key == "baked" and sources(cfg) == ["tmdb", "anilist"]
    cfg = config.load({**base, "TMDB_DEFAULT_API_KEY": "baked", "TMDB_API_KEY": "mine"})
    assert cfg.tmdb_api_key == "mine"
    cfg = config.load({**base, "TMDB_API_KEY": "mine", "METADATA_SOURCES": "anilist, tmdb"})
    assert sources(cfg) == ["anilist", "tmdb"]
    assert sources(config.load({**base, "METADATA_SOURCES": ""})) == []
    assert sources(config.load({**base, "METADATA_SOURCES": "none"})) == []
    with pytest.raises(SystemExit):
        config.load({**base, "METADATA_SOURCES": "imdb"})


def test_anilist_prefers_a_real_title_over_a_synonym():
    fake = FakeAnilist()
    fake.media.insert(0, {"id": 5, "format": "SPECIAL", "synonyms": ["Your Name."],
                          "title": {"romaji": "Some Commercial"}, "startDate": {"year": 2016}})
    fake.media.append({"id": 21519, "format": "MOVIE", "synonyms": [],
                       "title": {"romaji": "Kimi no Na wa.", "english": "Your Name."},
                       "startDate": {"year": 2016}})
    assert Anilist(fetch=fake).find("movie", "Your Name", 2016, None)["anilist_id"] == 21519
