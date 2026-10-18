"""Clean shutdown on SIGTERM/SIGINT.

`docker stop` (and idlegate, which stops the container after an idle period)
sends SIGTERM, waits 10 s, then SIGKILLs. Python as PID 1 has no SIGTERM
handler by default, so without this the container always ends with exit
code 137, which TrueNAS shows as Crashed. Exit 0 well inside the grace period.
"""

from __future__ import annotations

import atexit
import logging
import os
import signal
import sys
import threading

log = logging.getLogger("mediawatcher")
DEADLINE_S = 5  # half of docker stop's default 10 s grace

# Set once a stop signal arrives. Background work checks it to stop handing
# out items, and to avoid recording a failure that only happened because its
# ffmpeg/ffprobe child was killed along with us.
stopping = threading.Event()


def _force_exit() -> None:
    log.warning("shutdown still running after %ss, forcing exit", DEADLINE_S)
    logging.shutdown()
    os._exit(0)


def _kill_children() -> None:
    """SIGKILL our ffmpeg/ffprobe children, so a probe worker waiting on one
    finishes now instead of holding up the exit. Only SIGTERM reaches PID 1,
    not its children. Linux only; elsewhere the deadline still applies."""
    pids: set[int] = set()
    try:
        for tid in os.listdir("/proc/self/task"):
            with open(f"/proc/self/task/{tid}/children") as f:
                pids.update(int(p) for p in f.read().split())
    except OSError:
        return
    for pid in pids:
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass


def _on_signal(signum, _frame) -> None:
    if stopping.is_set():
        return  # second Ctrl-C: the deadline timer is already running
    stopping.set()
    log.info("received %s, shutting down", signal.Signals(signum).name)
    t = threading.Timer(DEADLINE_S, _force_exit)
    t.daemon = True
    t.start()
    _kill_children()
    sys.exit(0)  # SystemExit in the main thread: serve_forever unwinds, atexit runs


def install() -> None:
    """Call from the main thread before anything else starts."""
    signal.signal(signal.SIGTERM, _on_signal)
    signal.signal(signal.SIGINT, _on_signal)
    atexit.register(lambda: log.info("shutdown complete"))
