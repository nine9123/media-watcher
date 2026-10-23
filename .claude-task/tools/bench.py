"""Resource-usage benchmark of a running Media Watcher (stdlib only, Linux).

Used for the "Benchmarks" section of README.md. Plays each item for SECONDS at
roughly real-time speed (the download is capped at RATE bytes/s, about what a
browser pulls for an 8 Mbit/s file) and reports the CPU and memory of the
server process plus every ffmpeg it starts, sampled from /proc once a second.

    python3 .claude-task/tools/bench.py http://localhost:8097 KEY SERVER_PID \\
        DIRECT_ID REMUX_ID TRANSCODE_ID [SECONDS] [RATE]

The ids are items whose playback mode is direct, remux and transcode for a
client that reports no extra codecs (no vcaps/acaps): e.g. an H.264/AAC MP4,
an H.264/AC-3 MKV and an HEVC MKV. Run the server with AUTO_CONVERT=false so
no background conversion skews the numbers.
"""

import os
import sys
import threading
import time
import urllib.request

TICK = os.sysconf("SC_CLK_TCK")
PAGE = os.sysconf("SC_PAGE_SIZE")


def stat(pid):
    """(ppid, own cpu ticks, rss bytes) or None."""
    try:
        with open(f"/proc/{pid}/stat") as f:
            s = f.read()
    except OSError:
        return None
    fields = s[s.rindex(")") + 2:].split()
    ppid = int(fields[1])
    ticks = int(fields[11]) + int(fields[12])  # utime + stime
    return ppid, ticks, int(fields[21]) * PAGE


def tree(root):
    """{pid: (ticks, rss)} for root and all its descendants."""
    procs = {}
    for d in os.listdir("/proc"):
        if d.isdigit():
            st = stat(int(d))
            if st:
                procs[int(d)] = st
    out, todo = {}, [root]
    while todo:
        p = todo.pop()
        if p in procs:
            out[p] = procs[p][1:]
            todo += [c for c, st in procs.items() if st[0] == p]
    return out


def measure(root, seconds, work=None):
    stop = threading.Event()
    sent = [0]
    if work:
        t = threading.Thread(target=work, args=(stop, sent), daemon=True)
        t.start()
    first = tree(root)
    seen = dict(first)
    peak = 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        time.sleep(1)
        now = tree(root)
        seen.update(now)
        peak = max(peak, sum(r for _, r in now.values()))
    elapsed = time.time() - t0
    stop.set()
    # Each process is counted from its own ticks as last sampled, so a child
    # that exits mid-window loses at most its final second.
    ticks = sum(t for t, _ in seen.values()) - sum(t for t, _ in first.values())
    return ticks / TICK / elapsed * 100, peak, sent[0] / elapsed


def player(url, rate):
    def work(stop, sent):
        with urllib.request.urlopen(url) as r:
            t0 = time.time()
            while not stop.is_set():
                b = r.read(65536)
                if not b:
                    break
                sent[0] += len(b)
                ahead = sent[0] / rate - (time.time() - t0)
                if ahead > 0:
                    time.sleep(ahead)
    return work


def main():
    base, key, pid = sys.argv[1], sys.argv[2], int(sys.argv[3])
    direct, remux, transcode = sys.argv[4:7]
    seconds = int(sys.argv[7]) if len(sys.argv) > 7 else 60
    rate = int(sys.argv[8]) if len(sys.argv) > 8 else 1_250_000
    q = "api_key=" + key
    rows = [
        ("idle", None),
        ("direct play", player(f"{base}/file/{direct}?{q}", rate)),
        ("remux", player(f"{base}/stream/{remux}?{q}", rate)),
        ("transcode (CPU)", player(f"{base}/stream/{transcode}?{q}", rate)),
    ]
    print("| state | CPU (% of one core) | memory (RSS, server + ffmpeg) | sent |")
    print("|---|---|---|---|")
    for name, work in rows:
        cpu, rss, bps = measure(pid, seconds, work)
        print(f"| {name} | {cpu:.1f} % | {rss / 2**20:.0f} MB | {bps * 8 / 1e6:.1f} Mbit/s |", flush=True)
        time.sleep(3)  # let a stream's ffmpeg exit before the next row


if __name__ == "__main__":
    main()
