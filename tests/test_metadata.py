"""TMDB metadata against a fake API: matching, poster download, refresh rules."""

import json
import os
import shutil
import subprocess
import urllib.error
import urllib.parse

import pytest

from mediawatcher import metadata
from mediawatcher.config import Config, Library
from mediawatcher.db import Database
from mediawatcher.images import Images
from mediawatcher.metadata import Metadata, Tmdb, TmdbError, tmdb_id_from_path
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


@pytest.fixture
def env(tmp_path, monkeypatch):
    monkeypatch.setattr(metadata, "PAUSE", 0)
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
    meta = Metadata(cfg, db, images, Tmdb(cfg.tmdb_api_key, fetch=fake))
    scanner = Scanner(cfg, db, images, meta)
    scanner.scan()
    return {"cfg": cfg, "db": db, "images": images, "fake": fake, "meta": meta,
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
    env["scanner"].scan()
    assert env["fake"].calls == []


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
