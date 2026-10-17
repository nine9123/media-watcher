"""End-to-end tests against a real server on localhost.

ffprobe/ffmpeg are pointed at a missing binary: the scan, browsing, auth,
progress and byte-range paths must all work without them.
"""

import http.client
import json
import os
import shutil
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


def test_empty_show_folder_is_a_missing_series(env):
    os.makedirs(os.path.join(env["shows"], "Later Show (2024)", "Season 01"))
    os.makedirs(os.path.join(env["shows"], "Show"))  # same title as an existing series
    os.makedirs(os.path.join(env["movies"], "Empty Film (2001)"))
    env["scanner"].scan()
    lib = get_json(env, "/api/libraries/Shows")
    assert [(s["title"], s["episodes"]) for s in lib["series"]] == [("Later Show", 0), ("Show", 5)]
    assert get_json(env, "/api/libraries/Movies")["series"] == []
    assert {l["name"]: l["series"] for l in get_json(env, "/api/home")["libraries"]} \
        == {"Shows": 2, "Movies": 0}
    sid = lib["series"][0]["id"]
    s = get_json(env, f"/api/series/{sid}")
    assert (s["title"], s["year"], s["episodes"]) == ("Later Show", 2024, [])

    # Episodes arrive: same series, now with an episode.
    touch(env["shows"], "Later Show (2024)/Season 01/Later Show - S01E01.mkv")
    env["scanner"].scan()
    lib = get_json(env, "/api/libraries/Shows")
    assert (lib["series"][0]["id"], lib["series"][0]["episodes"]) == (sid, 1)

    # The folder is deleted: the series goes.
    shutil.rmtree(os.path.join(env["shows"], "Later Show (2024)"))
    env["scanner"].scan()
    assert [s["title"] for s in get_json(env, "/api/libraries/Shows")["series"]] == ["Show"]

    os.makedirs(os.path.join(env["shows"], "Gone Soon"))
    env["scanner"].scan()
    assert len(get_json(env, "/api/libraries/Shows")["series"]) == 2
    os.rmdir(os.path.join(env["shows"], "Gone Soon"))
    env["scanner"].scan()
    assert [s["title"] for s in get_json(env, "/api/libraries/Shows")["series"]] == ["Show"]


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


def test_home_recent_groups_series(env):
    home = get_json(env, "/api/home")
    assert sorted((r["type"], r["title"]) for r in home["recent"]) == [
        ("movie", "Heat"), ("series", "Show")]
    movie = next(r for r in home["recent"] if r["type"] == "movie")
    assert movie["file"] == "Heat (1995).mp4" and movie["watched"] is False


def test_db_pool_is_bounded(env):
    db = env["db"]
    errors = []

    def work():
        try:
            for _ in range(20):
                assert db.one("SELECT COUNT(*) AS n FROM items")["n"] == 6
        except Exception as e:  # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=work) for _ in range(12)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert errors == []
    assert db._opened <= db._pool_size


def test_cached_image_served_and_dropped_when_media_changes(env):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    img_dir = os.path.join(env["cfg"].cache_dir, "images")
    cached = os.path.join(img_dir, f"item-{mid}.jpg")
    assert req(env, "GET", f"/img/item/{mid}")[0].status == 404  # no ffmpeg, nothing cached
    with open(cached, "wb") as f:
        f.write(b"\xff\xd8jpeg")
    r, raw = req(env, "GET", f"/img/item/{mid}")
    assert r.status == 200 and raw == b"\xff\xd8jpeg"

    env["scanner"].scan()  # unchanged file: cache kept
    assert os.path.exists(cached)
    touch(env["movies"], "Heat (1995)/Heat (1995).mp4", b"new")
    env["scanner"].scan()
    assert not os.path.exists(cached)


def test_new_sidecar_poster_replaces_cached_series_image(env):
    sid = get_json(env, "/api/libraries/Shows")["series"][0]["id"]
    cached = os.path.join(env["cfg"].cache_dir, "images", f"series-{sid}.jpg")
    with open(cached, "wb") as f:
        f.write(b"old")
    touch(env["shows"], "Show (2020)/poster.jpg", b"poster")
    env["scanner"].scan()
    assert not os.path.exists(cached)


def test_nested_show_folders_group_into_one_series(tmp_path):
    anime = str(tmp_path / "media" / "Anime")
    touch(anime, "Show/Show S01/[Grp] Show - 01 [1080p].mkv")
    touch(anime, "Show/Show S01/[Grp] Show - 02 [1080p].mkv")
    touch(anime, "Show/Show S02/[Grp] Show - 01 [1080p].mkv")
    touch(anime, "Show/Show.S02E02.1080p-GRP/abc123.mkv")
    touch(anime, "Show/Pilot.mkv")  # no number, still part of the show
    touch(anime, "Other.S01.1080p-GRP/Other.S01E01.1080p-GRP.mkv")
    touch(anime, "Other.S02.1080p-GRP/Other.S02E01.1080p-GRP.mkv")
    touch(anime, "Lone Film (2001)/Lone Film (2001).mkv")
    cfg = Config(api_key=KEY, libraries=[Library("Anime", anime, "auto")],
                 cache_dir=str(tmp_path / "cache"), config_dir=str(tmp_path / "config"),
                 ffmpeg="/nonexistent/ffmpeg", ffprobe="/nonexistent/ffprobe")
    db = Database(cfg.cache_dir, cfg.config_dir)
    Scanner(cfg, db, Images(cfg, db)).scan_library(cfg.libraries[0])

    series = {r["title"]: r["id"] for r in db.query("SELECT id, title FROM series")}
    assert sorted(series) == ["Other", "Show"]
    eps = {r["rel_path"]: (r["series_id"], r["season"], r["episode"]) for r in db.query(
        "SELECT rel_path, series_id, season, episode FROM items WHERE kind='episode'")}
    assert eps["Show/Show S02/[Grp] Show - 01 [1080p].mkv"] == (series["Show"], 2, 1)
    assert eps["Show/Show.S02E02.1080p-GRP/abc123.mkv"] == (series["Show"], 2, 2)
    assert eps["Show/Pilot.mkv"][0] == series["Show"]
    assert eps["Other.S02.1080p-GRP/Other.S02E01.1080p-GRP.mkv"] == (series["Other"], 2, 1)
    movies = [r["title"] for r in db.query("SELECT title FROM items WHERE kind='movie'")]
    assert movies == ["Lone Film"]


def test_views_answer_304_until_the_database_changes(env):
    lib = "/api/libraries/Shows"
    r, raw = req(env, "GET", lib)
    etag = r.getheader("ETag")
    assert r.status == 200 and etag and r.getheader("Cache-Control") == "no-cache"
    r, raw = req(env, "GET", lib, headers={"If-None-Match": etag})
    assert r.status == 304 and raw == b""

    sid = json.loads(raw or req(env, "GET", lib)[1])["series"][0]["id"]
    eid = get_json(env, f"/api/series/{sid}")["episodes"][0]["id"]
    req(env, "POST", f"/api/items/{eid}/progress", {"position": 300, "duration": 1440})
    r, raw = req(env, "GET", lib, headers={"If-None-Match": etag})
    assert r.status == 200 and r.getheader("ETag") != etag
    assert json.loads(raw)["series"][0]["episodes"] == 5


def test_home_etag_follows_scan_status(env):
    r, _ = req(env, "GET", "/api/home")
    etag = r.getheader("ETag")
    assert req(env, "GET", "/api/home", headers={"If-None-Match": etag})[0].status == 304
    env["scanner"].status["error"] = "library Shows is missing"
    r, raw = req(env, "GET", "/api/home", headers={"If-None-Match": etag})
    assert r.status == 200 and json.loads(raw)["status"]["error"] == "library Shows is missing"


def test_playback_info_has_next_card_and_preferred_language(env):
    sid = get_json(env, "/api/libraries/Shows")["series"][0]["id"]
    e1, e2 = [e["id"] for e in get_json(env, f"/api/series/{sid}")["episodes"][:2]]
    streams = [
        {"index": 0, "type": "video", "codec": "h264", "pix_fmt": "yuv420p", "profile": "High"},
        {"index": 1, "type": "audio", "codec": "aac", "language": "jpn", "title": "", "channels": 2},
        {"index": 2, "type": "audio", "codec": "aac", "language": "eng", "title": "", "channels": 2},
    ]
    env["db"].write("UPDATE items SET probed=1, duration=1440, container='matroska,webm', "
                    "streams=? WHERE id=?", (json.dumps(streams), e1))
    info = get_json(env, f"/api/items/{e1}/playback")
    assert info["audio_index"] == 1  # AUDIO_LANGUAGES default: Japanese
    assert info["next"]["id"] == e2 and info["next"]["episode"] == 2
    info = get_json(env, f"/api/items/{e1}/playback?alang=eng")
    assert info["audio_index"] == 2 and "audio=2" in info["url"]


def test_failed_thumbnail_is_not_retried_until_the_file_changes(env):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    marker = os.path.join(env["cfg"].cache_dir, "images", f"item-{mid}.none")
    assert os.path.exists(marker)  # the scan tried once (no ffmpeg here) and gave up
    assert req(env, "GET", f"/img/item/{mid}")[0].status == 404

    calls = []
    images = env["scanner"].images
    images._frame = lambda *a: calls.append(a) or False
    env["scanner"].scan()
    assert calls == [] and os.path.exists(marker)

    touch(env["movies"], "Heat (1995)/Heat (1995).mp4", b"new")
    env["scanner"].scan()
    assert len(calls) == 1 and os.path.exists(marker)


def test_idle_keepalive_connection_is_closed(env, monkeypatch):
    from mediawatcher.server import Handler
    monkeypatch.setattr(Handler, "timeout", 0.3)
    c = http.client.HTTPConnection("127.0.0.1", env["port"], timeout=10)
    c.request("GET", "/healthz")
    c.getresponse().read()
    c.sock.settimeout(5)
    assert c.sock.recv(1) == b""  # server hung up instead of waiting for the next request
    c.close()


def test_next_up_follows_the_furthest_watched_episode(env):
    sid = get_json(env, "/api/libraries/Shows")["series"][0]["id"]
    eps = {(e["season"], e["episode"]): e["id"] for e in get_json(env, f"/api/series/{sid}")["episodes"]}
    req(env, "POST", f"/api/items/{eps[(0, 1)]}/watched", {"watched": True})
    assert get_json(env, "/api/home")["next_up"] == []  # a watched special starts nothing
    req(env, "POST", f"/api/items/{eps[(1, 2)]}/watched", {"watched": True})
    req(env, "POST", f"/api/items/{eps[(1, 3)]}/watched", {"watched": True})
    assert [n["id"] for n in get_json(env, "/api/home")["next_up"]] == [eps[(2, 1)]]
    req(env, "POST", f"/api/items/{eps[(2, 1)]}/watched", {"watched": True})
    assert get_json(env, "/api/home")["next_up"] == []  # E01 skipped long ago does not come back


def test_problems_lists_unreadable_files_and_missing_libraries(env):
    # ffprobe is missing in these tests, so every file failed to probe.
    home = get_json(env, "/api/home")
    assert home["problems"] == 6
    d = get_json(env, "/api/problems")
    assert len(d["files"]) == 6 and d["missing"] == []
    heat = next(f for f in d["files"] if f["library"] == "Movies")
    assert heat["path"] == "Heat (1995)/Heat (1995).mp4" and heat["error"]

    os.rename(env["movies"], env["movies"] + ".gone")
    env["scanner"].scan()
    d = get_json(env, "/api/problems")
    assert d["missing"] == [{"name": "Movies", "path": env["movies"]}]
    assert "Movies" in d["last_error"]


def test_playback_info_has_skips(env):
    mid = get_json(env, "/api/libraries/Movies")["movies"][0]["id"]
    streams = [{"index": 0, "type": "video", "codec": "h264", "pix_fmt": "yuv420p", "profile": "High"},
               {"index": 1, "type": "audio", "codec": "aac", "language": "", "title": "", "channels": 2}]
    skips = [{"kind": "intro", "start": 60.0, "end": 150.0}]
    env["db"].write("UPDATE items SET probed=1, duration=6000, container='mov,mp4,m4a,3gp,3g2,mj2', "
                    "streams=?, skips=? WHERE id=?", (json.dumps(streams), json.dumps(skips), mid))
    assert get_json(env, f"/api/items/{mid}/playback")["skips"] == skips


def test_old_library_db_gains_skips_and_is_reprobed(tmp_path):
    import sqlite3

    from mediawatcher.db import LIBRARY_SCHEMA
    cache = tmp_path / "cache"
    cache.mkdir()
    c = sqlite3.connect(cache / "library.db")
    c.executescript(LIBRARY_SCHEMA.replace(",\n    skips TEXT", ""))  # before skips existed
    c.executemany("INSERT INTO items(id, library, rel_path, path, size, mtime, kind, title, "
                  "sort_name, added_at, probed) VALUES(?,'L',?,?,1,1,'movie','t','t',1,?)",
                  [("a", "a", "/a", 1), ("b", "b", "/b", 2)])
    c.commit()
    c.close()
    db = Database(str(cache), str(tmp_path / "config"))
    rows = {r["id"]: r for r in db.query("SELECT id, probed, skips FROM items")}
    assert rows["a"]["probed"] == 0 and rows["b"]["probed"] == 2 and rows["a"]["skips"] is None
    Database(str(cache), str(tmp_path / "config"))  # second open: no reset again


def test_playback_info_suggests_converting_cpu_transcodes(env):
    sid = get_json(env, "/api/libraries/Shows")["series"][0]["id"]
    eps = get_json(env, f"/api/series/{sid}")["episodes"]
    s1 = [e for e in eps if e["season"] == 1]  # E01, E02 in "Season 01", loose 03 elsewhere
    hevc = [{"index": 0, "type": "video", "codec": "hevc", "pix_fmt": "yuv420p10le"},
            {"index": 1, "type": "audio", "codec": "aac", "language": "jpn", "title": "", "channels": 2}]
    for e in eps:  # every episode HEVC, but only Season 01 shares the folder
        env["db"].write("UPDATE items SET probed=1, duration=1440, container='matroska,webm', "
                        "streams=? WHERE id=?", (json.dumps(hevc), e["id"]))
    conv = get_json(env, f"/api/items/{s1[0]['id']}/playback")["convert"]
    assert conv["files"] == 2 and conv["folder"].endswith("Season 01")
    assert conv["folder_command"].splitlines()[0] == conv["file_command"]
    assert "libx264" in conv["file_command"]
    # Plays without a video transcode in a browser that decodes 10-bit HEVC.
    assert get_json(env, f"/api/items/{s1[0]['id']}/playback?vcaps=hevc,hevc10")["convert"] is None
    assert get_json(env, f"/api/items/{s1[0]['id']}/playback?force=transcode")["convert"] is None
    env["cfg"].hwaccel = "vaapi"
    assert get_json(env, f"/api/items/{s1[0]['id']}/playback")["convert"] is None
