"""Background conversion: one job per file, the cache limits, and a real
conversion through the HTTP server when ffmpeg is installed."""

import http.client
import json
import os
import shutil
import subprocess
import threading
import time

import pytest

from mediawatcher.config import Config, Library
from mediawatcher.converter import GB, Converter
from mediawatcher.db import Database
from mediawatcher.images import Images
from mediawatcher.scanner import Scanner
from mediawatcher.server import App, make_server

KEY = "secret-key-123"
STREAMS = [{"index": 0, "type": "video", "codec": "mpeg2video", "pix_fmt": "yuv420p", "height": 360},
           {"index": 1, "type": "audio", "codec": "ac3", "language": "jpn", "channels": 2}]


def conv(tmp_path, **kw):
    cfg = Config(api_key=KEY, libraries=[], cache_dir=str(tmp_path / "cache"),
                 ffmpeg="/nonexistent/ffmpeg", **kw)
    return Converter(cfg)


def source(tmp_path, name="a.mkv", data=b"x" * 100):
    p = tmp_path / name
    p.write_bytes(data)
    return {"path": str(p), "container": "matroska,webm", "duration": 60, "streams": STREAMS}


def fake_copy(c, iid, item, size, used_at):
    """A finished copy, as _run leaves it."""
    os.makedirs(c.dir, exist_ok=True)
    with open(c.path(iid), "wb") as f:
        f.truncate(size)
    st = os.stat(item["path"])
    with open(os.path.join(c.dir, f"{iid}.json"), "w") as f:
        json.dump({"title": iid, "name": "a.mp4", "source": item["path"], "source_size": st.st_size,
                   "source_mtime": int(st.st_mtime), "audio": [1], "created_at": used_at}, f)
    os.utime(c.path(iid), (used_at, used_at))


def test_same_file_is_queued_once(tmp_path):
    c = conv(tmp_path)
    c._ensure_thread = lambda: None  # keep the jobs queued
    it = source(tmp_path)
    first = c.request("a1", it, "A")
    again = c.request("a1", it, "A")
    assert first["state"] == again["state"] == "queued" and first["position"] == 1
    assert list(c.queue) == ["a1"] and len(c.jobs) == 1
    other = c.request("b2", source(tmp_path, "b.mkv"), "B")
    assert other["position"] == 2
    saved = json.load(open(os.path.join(c.dir, "queue.json")))
    assert [s["id"] for s in saved] == ["a1", "b2"]

    # After a restart the queue is picked up again, still one job per file.
    c2 = conv(tmp_path)
    c2._ensure_thread = lambda: None
    c2.start()
    assert list(c2.queue) == ["a1", "b2"]
    c2.request("a1", it, "A")
    assert list(c2.queue) == ["a1", "b2"]

    assert c2.cancel("a1") and list(c2.queue) == ["b2"] and "a1" not in c2.jobs


def test_failed_job_is_only_retried_on_request(tmp_path):
    c = conv(tmp_path)  # ffmpeg is missing, so every job fails
    it = source(tmp_path)
    c.request("a1", it, "A")
    deadline = time.time() + 5
    while c.status("a1")["state"] != "failed" and time.time() < deadline:
        time.sleep(0.02)
    st = c.status("a1")
    assert st["state"] == "failed" and "ffmpeg" in st["error"]
    assert c.request("a1", it, "A")["state"] == "failed"  # opening it again does not re-run
    assert c.request("a1", it, "A", retry=True)["state"] in ("queued", "running", "failed")
    assert c.summary()["queued"] <= 1


def test_cached_copy_is_used_until_the_source_changes(tmp_path):
    c = conv(tmp_path)
    it = source(tmp_path)
    fake_copy(c, "a1", it, 10, time.time())
    st = c.request("a1", it, "A")
    assert st["state"] == "done" and st["audio_index"] == 1 and st["url"] == "/converted/a1"
    assert not c.jobs
    with open(it["path"], "ab") as f:
        f.write(b"more")  # replaced or re-encoded by hand
    assert c.status("a1", it["path"]) is None
    assert not os.path.exists(c.path("a1"))


def test_expired_and_least_recently_played_copies_are_dropped(tmp_path):
    c = conv(tmp_path, convert_cache_days=14, convert_cache_gb=25 / GB)  # 25 bytes
    now = time.time()
    it = source(tmp_path)
    fake_copy(c, "old", it, 5, now - 15 * 86400)
    fake_copy(c, "lru", it, 10, now - 3600)
    fake_copy(c, "new", it, 10, now - 60)
    fake_copy(c, "newer", it, 10, now)
    c.cleanup()
    assert sorted(f["id"] for f in c.overview()["files"]) == ["new", "newer"]
    c.touch("new")  # played again: now the most recent
    c._make_room(10)
    assert [f["id"] for f in c.overview()["files"]] == ["new"]


# ---- real conversion through the server -----------------------------------------

needs_ffmpeg = pytest.mark.skipif(not (shutil.which("ffmpeg") and shutil.which("ffprobe")),
                                  reason="ffmpeg/ffprobe not installed")


def req(port, method, path, body=None):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    data = json.dumps(body).encode() if body is not None else None
    c.request(method, path, body=data, headers={"X-Api-Key": KEY})
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r, raw


def get_json(port, path):
    r, raw = req(port, "GET", path)
    assert r.status == 200, raw
    return json.loads(raw)


@needs_ffmpeg
def test_mpeg2_is_converted_once_then_played_directly(tmp_path):
    movies = tmp_path / "media" / "Movies"
    src = movies / "Old (1990)" / "Old (1990).mkv"
    src.parent.mkdir(parents=True)
    subprocess.run(
        ["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=25",
         "-f", "lavfi", "-i", "sine=frequency=440", "-f", "lavfi", "-i", "sine=frequency=880",
         "-t", "4", "-map", "0", "-map", "1", "-map", "2", "-c:v", "mpeg2video", "-c:a", "ac3",
         "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn", str(src)],
        check=True)
    cfg = Config(api_key=KEY, libraries=[Library("Movies", str(movies), "movies")],
                 cache_dir=str(tmp_path / "cache"), config_dir=str(tmp_path / "config"),
                 host="127.0.0.1", port=0,
                 web_dir=os.path.join(os.path.dirname(__file__), "..", "web"))
    db = Database(cfg.cache_dir, cfg.config_dir)
    images = Images(cfg, db)
    scanner = Scanner(cfg, db, images)
    scanner.scan()
    server = make_server(App(cfg, db, images, scanner))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    port = server.server_address[1]
    try:
        mid = get_json(port, "/api/libraries/Movies")["movies"][0]["id"]
        info = get_json(port, f"/api/items/{mid}/playback")
        assert info["mode"] == "transcode"  # still playable while the copy is made
        assert info["conversion"]["state"] in ("queued", "running", "done")
        assert info["conversion"]["title"] == "Old (1990)"
        get_json(port, f"/api/items/{mid}/playback")  # a second visit: no second job

        deadline = time.time() + 60
        while time.time() < deadline:
            st = get_json(port, f"/api/conversions/{mid}")["conversion"]
            if st["state"] not in ("queued", "running"):
                break
            time.sleep(0.2)
        assert st["state"] == "done", st
        assert get_json(port, "/api/status")["converting"] == {"queued": 0, "running": None,
                                                              "files": 1}

        info = get_json(port, f"/api/items/{mid}/playback")
        assert info["mode"] == "direct" and info["url"] == f"/converted/{mid}"
        assert info["audio_index"] == 2  # Japanese, the copy's first track
        # Another audio track than the copy's first: back to the original.
        assert get_json(port, f"/api/items/{mid}/playback?audio=1")["mode"] == "transcode"

        r, body = req(port, "GET", f"/converted/{mid}?download=1")
        assert r.status == 200 and r.getheader("Accept-Ranges") == "bytes"
        assert "Old%20%281990%29.mp4" in r.getheader("Content-Disposition")
        out = tmp_path / "out.mp4"
        out.write_bytes(body)
        probe = json.loads(subprocess.run(
            ["ffprobe", "-v", "error", "-show_streams", "-of", "json", str(out)],
            check=True, capture_output=True).stdout)["streams"]
        # Japanese first and the default, so a browser plays it; English kept as is.
        assert [(s["codec_name"], s.get("tags", {}).get("language"), s["disposition"]["default"])
                for s in probe] == [("h264", "und", 1), ("aac", "jpn", 1), ("ac3", "eng", 0)]
        assert probe[0]["pix_fmt"] == "yuv420p"

        files = get_json(port, "/api/conversions")["files"]
        assert [f["id"] for f in files] == [mid] and files[0]["size"] == len(body)
        assert req(port, "POST", f"/api/conversions/{mid}/delete", {})[0].status == 200
        assert req(port, "GET", f"/converted/{mid}")[0].status == 404
    finally:
        server.shutdown()
        server.server_close()
