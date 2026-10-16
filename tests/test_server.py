"""End-to-end tests against a real server on localhost.

ffprobe/ffmpeg are pointed at a missing binary: the scan, browsing, auth,
progress and byte-range paths must all work without them.
"""

import http.client
import json
import os
import threading

import pytest

from mediawatcher.config import Config, Library
from mediawatcher.db import Database
from mediawatcher.images import Images
from mediawatcher.scanner import Scanner
from mediawatcher.server import App, make_server

KEY = "secret-key-123"


def touch(root, rel, data=b"\0" * 2048):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    return path


@pytest.fixture
def env(tmp_path):
    media = tmp_path / "media"
    shows, movies = str(media / "Shows"), str(media / "Movies")
    touch(shows, "Show (2020)/Season 01/Show - S01E01 - One.mkv")
    touch(shows, "Show (2020)/Season 01/Show - S01E02 - Two.mkv")
    touch(shows, "Show (2020)/Season 02/Show - S02E01.mkv")
    touch(shows, "Show (2020)/Specials/Show - S00E01.mkv")
    touch(shows, "Show (2020)/Season 01/Show - S01E01 - One-sample.mkv")
    touch(shows, "Show (2020)/@eaDir/Show - S01E01.mkv")
    touch(shows, "[Grp] Show - 03 [1080p].mkv")  # loose file joins "Show"
    touch(movies, "Heat (1995)/Heat (1995).mp4", bytes(range(256)) * 40)
    cfg = Config(
        api_key=KEY,
        libraries=[Library("Shows", shows, "shows"), Library("Movies", movies, "movies")],
        cache_dir=str(tmp_path / "cache"), config_dir=str(tmp_path / "config"),
        host="127.0.0.1", port=0, ffmpeg="/nonexistent/ffmpeg", ffprobe="/nonexistent/ffprobe",
        web_dir=os.path.join(os.path.dirname(__file__), "..", "web"),
    )
    db = Database(cfg.cache_dir, cfg.config_dir)
    images = Images(cfg, db)
    scanner = Scanner(cfg, db, images)
    scanner.scan()
    server = make_server(App(cfg, db, images, scanner))
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    yield {"port": server.server_address[1], "cfg": cfg, "db": db, "scanner": scanner,
           "shows": shows, "movies": movies}
    server.shutdown()
    server.server_close()


def req(env, method, path, body=None, headers=None, key=KEY):
    c = http.client.HTTPConnection("127.0.0.1", env["port"], timeout=10)
    h = dict(headers or {})
    if key:
        h["X-Api-Key"] = key
    data = json.dumps(body).encode() if body is not None else None
    if data:
        h["Content-Type"] = "application/json"
    c.request(method, path, body=data, headers=h)
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r, raw


def get_json(env, path):
    r, raw = req(env, "GET", path)
    assert r.status == 200, raw
    return json.loads(raw)


def test_auth(env):
    assert req(env, "GET", "/api/home", key=None)[0].status == 401
    assert req(env, "GET", "/api/home", key="wrong")[0].status == 401
    assert req(env, "GET", "/healthz", key=None)[0].status == 200
    r, _ = req(env, "GET", "/", key=None)
    assert r.status == 200  # UI shell itself is public, data is not
    r, _ = req(env, "POST", "/api/login", {"key": KEY}, key=None)
    assert r.status == 200
    cookie = r.getheader("Set-Cookie").split(";")[0]
    assert KEY not in cookie
    r, _ = req(env, "GET", "/api/home", headers={"Cookie": cookie}, key=None)
    assert r.status == 200
    r, _ = req(env, "GET", "/api/home?api_key=" + KEY, key=None)
    assert r.status == 200


def test_library_grouping_and_cleanup(env):
    lib = get_json(env, "/api/libraries/Shows")
    assert [s["title"] for s in lib["series"]] == ["Show"]
    assert lib["series"][0]["episodes"] == 5  # sample + @eaDir skipped, loose file merged
    assert lib["movies"] == []
    movies = get_json(env, "/api/libraries/Movies")["movies"]
    assert [(m["title"], m["year"]) for m in movies] == [("Heat", 1995)]

    series = get_json(env, f"/api/series/{lib['series'][0]['id']}")
    order = [(e["season"], e["episode"]) for e in series["episodes"]]
    assert order == [(1, 1), (1, 2), (1, 3), (2, 1), (0, 1)]  # specials last
    assert series["next_id"] == series["episodes"][0]["id"]

    os.remove(os.path.join(env["shows"], "Show (2020)/Season 02/Show - S02E01.mkv"))
    env["scanner"].scan()
    lib = get_json(env, "/api/libraries/Shows")
    assert lib["series"][0]["episodes"] == 4


def test_unmounted_share_does_not_wipe_library(env):
    for dirpath, _, files in os.walk(env["movies"]):
        for f in files:
            os.remove(os.path.join(dirpath, f))
    env["scanner"].scan()
    assert len(get_json(env, "/api/libraries/Movies")["movies"]) == 1


def test_progress_continue_and_next_up(env):
    sid = get_json(env, "/api/libraries/Shows")["series"][0]["id"]
    eps = get_json(env, f"/api/series/{sid}")["episodes"]
    e1, e2 = eps[0]["id"], eps[1]["id"]

    r, raw = req(env, "POST", f"/api/items/{e1}/progress", {"position": 300, "duration": 1440})
    assert json.loads(raw) == {"watched": False}
    home = get_json(env, "/api/home")
    assert [c["id"] for c in home["continue"]] == [e1]

    r, raw = req(env, "POST", f"/api/items/{e1}/progress", {"position": 1350, "duration": 1440})
    assert json.loads(raw) == {"watched": True}
    home = get_json(env, "/api/home")
    assert home["continue"] == []
    assert [n["id"] for n in home["next_up"]] == [e2]
    assert get_json(env, f"/api/series/{sid}")["next_id"] == e2

    req(env, "POST", f"/api/series/{sid}/watched", {"watched": True, "season": 1})
    eps = get_json(env, f"/api/series/{sid}")["episodes"]
    assert [e["watched"] for e in eps] == [True, True, True, False, False]


def test_progress_survives_cache_wipe(env, tmp_path):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    req(env, "POST", f"/api/items/{mid}/progress", {"position": 600, "duration": 6000})
    db2 = Database(str(tmp_path / "cache2"), env["cfg"].config_dir)
    row = db2.one("SELECT position FROM st.progress WHERE id=?", (mid,))
    assert row["position"] == 600


def test_byte_ranges(env):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    data = bytes(range(256)) * 40
    r, raw = req(env, "GET", f"/file/{mid}")
    assert r.status == 200 and raw == data and r.getheader("Accept-Ranges") == "bytes"
    r, raw = req(env, "GET", f"/file/{mid}", headers={"Range": "bytes=10-19"})
    assert r.status == 206 and raw == data[10:20]
    assert r.getheader("Content-Range") == f"bytes 10-19/{len(data)}"
    r, raw = req(env, "GET", f"/file/{mid}", headers={"Range": "bytes=-5"})
    assert r.status == 206 and raw == data[-5:]
    r, raw = req(env, "GET", f"/file/{mid}", headers={"Range": f"bytes={len(data)}-"})
    assert r.status == 416


def test_keepalive_survives_error_with_body(env):
    c = http.client.HTTPConnection("127.0.0.1", env["port"], timeout=10)
    c.request("POST", "/api/items/0000/progress", body=b'{"position": 1}',
              headers={"Content-Type": "application/json"})  # 401, body unread by handler
    r = c.getresponse()
    r.read()
    assert r.status == 401
    c.request("GET", "/api/home", headers={"X-Api-Key": KEY})
    r = c.getresponse()
    r.read()
    assert r.status == 200
    c.close()


def test_unprobeable_file_reports_error(env):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    r, raw = req(env, "GET", f"/api/items/{mid}/playback")
    assert r.status == 500 and b"cannot read file" in raw


def test_search(env):
    d = get_json(env, "/api/search?q=hea")
    assert [m["title"] for m in d["movies"]] == ["Heat"]
    d = get_json(env, "/api/search?q=sho")
    assert [s["title"] for s in d["series"]] == ["Show"]
