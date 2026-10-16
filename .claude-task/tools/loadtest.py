"""Tiny HTTP load generator (stdlib only) used for docs/stack-evaluation.md.

Starts CONCURRENCY threads that each send REQUESTS sequential GETs to URL,
then prints requests/s and p50/p99 latency. Run it against a local server
and read the server's RSS from /proc/<pid>/status (VmRSS / VmHWM) before and
after.

    python3 .claude-task/tools/loadtest.py "http://localhost:8097/api/home?api_key=KEY" 20 10
"""

import sys
import threading
import time
import urllib.request


def main() -> None:
    url, conc, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
    lat: list[float] = []

    def worker() -> None:
        for _ in range(n):
            t = time.time()
            urllib.request.urlopen(url).read()
            lat.append(time.time() - t)

    t0 = time.time()
    threads = [threading.Thread(target=worker) for _ in range(conc)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.time() - t0
    lat.sort()
    print(f"conc={conc} reqs={len(lat)} rps={len(lat) / elapsed:.0f} "
          f"p50={lat[len(lat) // 2] * 1000:.1f}ms p99={lat[int(len(lat) * .99)] * 1000:.1f}ms")


if __name__ == "__main__":
    main()
