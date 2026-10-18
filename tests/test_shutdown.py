"""The real entry point must exit 0 soon after SIGTERM (docker stop, idlegate)
and SIGINT (Ctrl-C), idle and mid-scan.

The subprocess is not PID 1, so this can't catch a shell-form CMD in the
Dockerfile; it does catch a removed or broken signal handler.
"""

import os
import signal
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.request

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def start(tmp_path, ffprobe="/nonexistent/ffprobe"):
    media = tmp_path / "media" / "Movies"
    media.mkdir(parents=True)
    (media / "Film (2001).mkv").write_bytes(b"\0" * 2048)
    port = free_port()
    env = dict(os.environ, API_KEY="secret-key-123", MEDIA_ROOT=str(tmp_path / "media"),
               CACHE_DIR=str(tmp_path / "cache"), CONFIG_DIR=str(tmp_path / "config"),
               HOST="127.0.0.1", PORT=str(port), FFPROBE=ffprobe,
               FFMPEG="/nonexistent/ffmpeg", METADATA_SOURCES="")
    # Own session, so leftover children (the fake ffprobe) can be cleaned up.
    proc = subprocess.Popen([sys.executable, "-m", "mediawatcher"], cwd=ROOT, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            start_new_session=True)
    deadline = time.time() + 15
    while True:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as r:
                assert r.status == 200
                return proc
        except OSError:
            if proc.poll() is not None or time.time() > deadline:
                proc.kill()
                pytest.fail("server did not come up:\n" + proc.stdout.read().decode())
            time.sleep(0.1)


def stop(proc, sig, timeout):
    t0 = time.time()
    proc.send_signal(sig)
    try:
        code = proc.wait(timeout=timeout)
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    return code, time.time() - t0, proc.stdout.read().decode()


@pytest.mark.parametrize("sig", [signal.SIGTERM, signal.SIGINT])
def test_signal_exits_zero_when_idle(tmp_path, sig):
    proc = start(tmp_path)
    code, took, log = stop(proc, sig, timeout=6)
    assert code == 0, log
    assert took < 3, log
    assert f"received {sig.name}, shutting down" in log
    assert "shutdown complete" in log


def test_sigterm_mid_scan_exits_zero_and_leaves_item_for_later(tmp_path):
    fake = tmp_path / "ffprobe"
    fake.write_text("#!/bin/sh\nexec sleep 30\n")
    fake.chmod(0o755)
    proc = start(tmp_path, ffprobe=str(fake))
    time.sleep(1)  # let the scanner reach the probe step
    code, took, log = stop(proc, signal.SIGTERM, timeout=8)
    assert code == 0, log
    assert took < 3, log
    db = sqlite3.connect(tmp_path / "cache" / "library.db")
    # Interrupted, not failed: probed again on the next start.
    assert db.execute("SELECT probed, probe_error FROM items").fetchall() == [(0, None)]
