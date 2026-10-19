"""Background conversion of files the browser cannot play into a cached MP4.

A file that needs a CPU transcode costs a full encode on every play. The
first time one is played it is queued here instead: one ffmpeg at a time,
at low priority, writes an 8-bit H.264 MP4 (see playback.converted_args)
into CACHE_DIR/converted. Once that exists the player uses it as a
direct-play file, and it can be downloaded to replace the original.

  * One job per item. Opening the same file again, from any browser, gets
    the job that is already queued or running, never a second one.
  * Copies not played for `convert_cache_days` are deleted, and the least
    recently played go first when the cache is over `convert_cache_gb`.
    The file's mtime is its last-played time.
  * A copy whose source file changed (size or mtime) is dropped.
  * Queued jobs survive a restart (queue.json); a job that was running
    starts over, ffmpeg cannot resume a half-written MP4.
"""

from __future__ import annotations

import collections
import json
import logging
import os
import shutil
import subprocess
import threading
import time
from dataclasses import dataclass, field

from . import playback, shutdown
from .config import Config

log = logging.getLogger("mediawatcher.convert")

CLEANUP_EVERY = 3600.0  # seconds between expiry sweeps while idle
GB = 1024 ** 3


@dataclass
class Job:
    id: str
    title: str
    file: str  # original file name, for display
    item: dict  # path, container, duration, streams
    state: str = "queued"  # queued | running | failed
    queued_at: float = field(default_factory=time.time)
    started_at: float | None = None
    done_s: float = 0.0  # seconds of the source encoded so far
    speed: float | None = None  # ffmpeg's realtime factor
    error: str | None = None
    proc: subprocess.Popen | None = None
    cancelled: bool = False


class Converter:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.dir = os.path.join(cfg.cache_dir, "converted")
        self.cv = threading.Condition()
        self.jobs: dict[str, Job] = {}  # queued, running and failed; done ones are files
        self.queue: collections.deque[str] = collections.deque()
        self.thread: threading.Thread | None = None
        self.last_speed: float | None = None  # for estimating queued jobs

    # ---- paths and cached copies --------------------------------------------
    @property
    def max_bytes(self) -> int:
        return int(self.cfg.convert_cache_gb * GB)

    @property
    def max_age(self) -> float:
        return self.cfg.convert_cache_days * 86400

    def path(self, iid: str) -> str:
        return os.path.join(self.dir, f"{iid}.mp4")

    def _meta_path(self, iid: str) -> str:
        return os.path.join(self.dir, f"{iid}.json")

    def _remove(self, iid: str) -> None:
        for p in (self.path(iid), self._meta_path(iid)):
            try:
                os.remove(p)
            except FileNotFoundError:
                pass

    def cached(self, iid: str, source: str | None = None) -> dict | None:
        """Metadata of a finished copy, or None. With `source`, a copy made
        from a different version of that file is deleted and None returned."""
        try:
            with open(self._meta_path(iid)) as f:
                meta = json.load(f)
            st = os.stat(self.path(iid))
        except (OSError, ValueError):
            return None
        if source is not None:
            try:
                src = os.stat(source)
            except OSError:
                return None  # share not mounted: keep the copy, just don't use it
            if (src.st_size, int(src.st_mtime)) != (meta.get("source_size"), meta.get("source_mtime")):
                log.info("dropping converted copy of %s: the file changed", source)
                self._remove(iid)
                return None
        meta.update(size=st.st_size, used_at=st.st_mtime)
        return meta

    def touch(self, iid: str) -> None:
        """Mark a copy as played now: it expires `convert_cache_days` from here."""
        try:
            os.utime(self.path(iid))
        except OSError:
            pass

    # ---- jobs ----------------------------------------------------------------
    def request(self, iid: str, item: dict, title: str, retry: bool = False) -> dict:
        """Status of the copy of `item`, queueing a conversion if there is none.

        A failed job is only tried again with `retry`, so a file ffmpeg cannot
        convert is not re-encoded every time someone opens it.
        """
        with self.cv:
            job = self.jobs.get(iid)
            if job and (job.state != "failed" or not retry):
                return self._job_json(job)
        meta = self.cached(iid, item["path"])
        if meta:
            return self._cached_json(iid, meta)
        with self.cv:
            job = self.jobs.get(iid)  # a second request may have got here first
            if job and job.state != "failed":
                return self._job_json(job)
            job = Job(iid, title, os.path.basename(item["path"]),
                      {k: item.get(k) for k in ("path", "container", "duration", "streams")})
            self.jobs[iid] = job
            self.queue.append(iid)
            self._save_queue()
            self._ensure_thread()
            self.cv.notify_all()
            log.info("queued conversion of %s", item["path"])
            return self._job_json(job)

    def status(self, iid: str, source: str | None = None) -> dict | None:
        with self.cv:
            job = self.jobs.get(iid)
            if job:
                return self._job_json(job)
        meta = self.cached(iid, source)
        return self._cached_json(iid, meta) if meta else None

    def cancel(self, iid: str) -> bool:
        with self.cv:
            job = self.jobs.pop(iid, None)
            if job is None:
                return False
            job.cancelled = True
            if iid in self.queue:
                self.queue.remove(iid)
            proc = job.proc
            self._save_queue()
        if proc is not None and proc.poll() is None:
            proc.kill()
        log.info("cancelled conversion of %s", job.file)
        return True

    def delete(self, iid: str) -> None:
        """Cancel any job and delete the copy."""
        self.cancel(iid)
        self._remove(iid)

    def summary(self) -> dict:
        """Small enough for every status poll: what is converting right now."""
        with self.cv:
            running = next((j for j in self.jobs.values() if j.state == "running"), None)
            out = {"queued": sum(j.state == "queued" for j in self.jobs.values()),
                   "running": self._job_json(running) if running else None}
        try:
            out["files"] = sum(n.endswith(".mp4") for n in os.listdir(self.dir))
        except OSError:
            out["files"] = 0
        return out

    def overview(self) -> dict:
        with self.cv:
            jobs = sorted(self.jobs.values(), key=lambda j: (
                {"running": 0, "queued": 1, "failed": 2}[j.state],
                list(self.queue).index(j.id) if j.id in self.queue else 0, j.queued_at))
            jobs = [self._job_json(j) for j in jobs]
        files = [self._cached_json(iid, m) for iid, m in self._all_cached()]
        files.sort(key=lambda f: -f["used_at"])
        return {"jobs": jobs, "files": files, "used": sum(f["size"] for f in files),
                "max": self.max_bytes, "days": self.cfg.convert_cache_days,
                "auto": self.cfg.auto_convert}

    # ---- status json -----------------------------------------------------------
    def _job_json(self, job: Job) -> dict:
        """Caller holds self.cv."""
        dur = (job.item.get("duration") or 0) or 0
        out = {"id": job.id, "title": job.title, "file": job.file, "state": job.state,
               "error": job.error, "progress": 0.0, "eta": None, "speed": job.speed,
               "elapsed": None, "position": None}
        if job.state == "running":
            elapsed = time.time() - (job.started_at or time.time())
            p = min(1.0, job.done_s / dur) if dur else 0.0
            out.update(progress=p, elapsed=elapsed, eta=self._remaining(job, elapsed, p))
        elif job.state == "queued":
            q = list(self.queue)
            pos = q.index(job.id) if job.id in q else 0
            out["position"] = pos + 1
            # Rough: what is left of the running job, then each queued one at
            # the speed the last job ran at (or the running one runs at).
            wait: float | None = 0.0
            running = next((j for j in self.jobs.values() if j.state == "running"), None)
            if running:
                rdur = running.item.get("duration") or 0
                relapsed = time.time() - (running.started_at or time.time())
                wait = self._remaining(running, relapsed,
                                       min(1.0, running.done_s / rdur) if rdur else 0.0)
            speed = self.last_speed or (running.speed if running else None)
            for other in q[:pos + 1]:
                d = self.jobs[other].item.get("duration") or 0
                if wait is None or not speed or not d:
                    wait = None
                    break
                wait += d / speed
            out["eta"] = wait
        return out

    def _remaining(self, job: Job, elapsed: float, p: float) -> float | None:
        if p >= 0.01 and elapsed >= 5:
            return max(0.0, elapsed * (1 - p) / p)
        dur = job.item.get("duration") or 0
        if job.speed and dur:
            return max(0.0, (dur - job.done_s) / job.speed)
        return None

    def _cached_json(self, iid: str, meta: dict) -> dict:
        return {"id": iid, "title": meta.get("title"), "file": meta.get("name"), "state": "done",
                "size": meta["size"], "used_at": meta["used_at"],
                "created_at": meta.get("created_at"),
                "expires_at": meta["used_at"] + self.max_age,
                "audio_index": (meta.get("audio") or [None])[0],
                "url": f"/converted/{iid}", "download": f"/converted/{iid}?download=1"}

    # ---- worker ----------------------------------------------------------------
    def start(self) -> None:
        """Pick up jobs queued before a restart and start the worker."""
        os.makedirs(self.dir, exist_ok=True)
        for name in os.listdir(self.dir):
            if name.endswith(".part"):  # from a conversion cut off by a restart
                try:
                    os.remove(os.path.join(self.dir, name))
                except OSError:
                    pass
        try:
            with open(os.path.join(self.dir, "queue.json")) as f:
                saved = json.load(f)
        except (OSError, ValueError):
            saved = []
        with self.cv:
            for s in saved:
                if s["id"] not in self.jobs:
                    self.jobs[s["id"]] = Job(s["id"], s["title"], s["file"], s["item"])
                    self.queue.append(s["id"])
            if self.queue:
                log.info("resuming %d queued conversion(s)", len(self.queue))
            self._ensure_thread()

    def _ensure_thread(self) -> None:
        """Caller holds self.cv."""
        if self.thread is None or not self.thread.is_alive():
            self.thread = threading.Thread(target=self._loop, name="converter", daemon=True)
            self.thread.start()

    def _save_queue(self) -> None:
        """Caller holds self.cv. Running first, so it starts first after a restart."""
        pending = sorted((j for j in self.jobs.values() if j.state in ("queued", "running")),
                         key=lambda j: (j.state != "running", j.queued_at))
        data = [{"id": j.id, "title": j.title, "file": j.file, "item": j.item} for j in pending]
        try:
            os.makedirs(self.dir, exist_ok=True)
            tmp = os.path.join(self.dir, "queue.json.tmp")
            with open(tmp, "w") as f:
                json.dump(data, f)
            os.replace(tmp, os.path.join(self.dir, "queue.json"))
        except OSError as e:
            log.warning("cannot save conversion queue: %s", e)

    def _loop(self) -> None:
        self.cleanup()
        while not shutdown.stopping.is_set():
            with self.cv:
                if not self.queue:
                    self.cv.wait(timeout=CLEANUP_EVERY)
                job = None
                while self.queue and job is None:
                    job = self.jobs.get(self.queue.popleft())
                if job is not None:
                    job.state, job.started_at, job.error = "running", time.time(), None
            if job is None:
                self.cleanup()
                continue
            try:
                self._run(job)
            except Exception as e:  # noqa: BLE001
                log.exception("conversion of %s failed", job.file)
                self._fail(job, str(e))
            self.cleanup()

    def _fail(self, job: Job, error: str) -> None:
        with self.cv:
            job.proc = None
            if self.jobs.get(job.id) is job:
                job.state, job.error = "failed", error
            self._save_queue()

    def _finish(self, job: Job) -> None:
        with self.cv:
            job.proc = None
            if self.jobs.get(job.id) is job:
                del self.jobs[job.id]
            self._save_queue()

    def _run(self, job: Job) -> None:
        src = job.item["path"]
        try:
            st = os.stat(src)
        except OSError as e:
            self._fail(job, f"cannot read file: {e.strerror}")
            return
        os.makedirs(self.dir, exist_ok=True)
        self._make_room(min(st.st_size, self.max_bytes), keep=job.id)
        part = self.path(job.id) + ".part"
        args = playback.converted_args(self.cfg, job.item, self.cfg.audio_languages, part)
        if shutil.which("nice"):
            args = ["nice", "-n", "10"] + args  # live streams come first
        log.info("converting %s", src)
        try:
            proc = subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, text=True, bufsize=1)
        except OSError as e:
            self._fail(job, f"cannot start ffmpeg: {e}")
            return
        with self.cv:
            job.proc = proc
            cancelled = job.cancelled
        if cancelled:
            proc.kill()
        errors: collections.deque[str] = collections.deque(maxlen=20)

        def drain():
            for line in proc.stderr:
                errors.append(line)

        threading.Thread(target=drain, daemon=True).start()
        for line in proc.stdout:
            key, _, value = line.strip().partition("=")
            if key == "out_time_us" and value.lstrip("-").isdigit():
                job.done_s = max(0.0, int(value) / 1e6)
            elif key == "speed" and value.endswith("x"):
                try:
                    job.speed = float(value[:-1]) or None
                except ValueError:
                    pass
        proc.wait()
        if job.cancelled or shutdown.stopping.is_set():
            _unlink(part)
            if not job.cancelled:
                return  # stays in queue.json, starts again after the restart
            self._finish(job)
            return
        if proc.returncode != 0:
            _unlink(part)
            msg = "".join(errors).strip() or f"ffmpeg exited with {proc.returncode}"
            log.warning("conversion of %s failed: %s", src, msg)
            self._fail(job, msg[-500:])
            return
        try:
            st2 = os.stat(src)
        except OSError:
            st2 = None
        if st2 is None or (st2.st_size, int(st2.st_mtime)) != (st.st_size, int(st.st_mtime)):
            _unlink(part)
            self._fail(job, "the file changed while it was being converted")
            return
        size = os.path.getsize(part)
        if size > self.max_bytes:
            _unlink(part)
            self._fail(job, f"the converted file ({size / GB:.1f} GB) is larger than the whole "
                            f"conversion cache ({self.cfg.convert_cache_gb:g} GB)")
            return
        elapsed = time.time() - (job.started_at or time.time())
        dur = job.item.get("duration") or 0
        if dur and elapsed > 0:
            self.last_speed = dur / elapsed
        os.replace(part, self.path(job.id))
        name = os.path.splitext(job.file)[0] + ".mp4"
        audio = [a["index"] for a in playback.converted_audio_order(
            job.item.get("streams") or [], self.cfg.audio_languages)]
        meta = {"title": job.title, "name": name, "source": src, "source_size": st.st_size,
                "source_mtime": int(st.st_mtime), "audio": audio, "created_at": time.time()}
        with open(self._meta_path(job.id) + ".tmp", "w") as f:
            json.dump(meta, f)
        os.replace(self._meta_path(job.id) + ".tmp", self._meta_path(job.id))
        log.info("converted %s in %.0fs (%.1f MB)", src, elapsed, size / 1e6)
        self._finish(job)

    # ---- cache limits ----------------------------------------------------------
    def _all_cached(self) -> list[tuple[str, dict]]:
        try:
            names = os.listdir(self.dir)
        except OSError:
            return []
        out = []
        for name in names:
            if name.endswith(".mp4"):
                iid = name[:-4]
                meta = self.cached(iid)
                if meta:
                    out.append((iid, meta))
                elif not os.path.exists(self._meta_path(iid)):
                    _unlink(os.path.join(self.dir, name))  # copy without metadata: unusable
        return out

    def cleanup(self) -> None:
        """Delete expired copies, then the least recently played ones over the limit."""
        now = time.time()
        for iid, meta in self._all_cached():
            if now - meta["used_at"] > self.max_age:
                log.info("converted copy of %s expired", meta.get("source"))
                self._remove(iid)
        self._make_room(0)

    def _make_room(self, need: int, keep: str | None = None) -> None:
        files = sorted(self._all_cached(), key=lambda x: x[1]["used_at"])
        total = sum(m["size"] for _, m in files)
        for iid, meta in files:
            if total + need <= self.max_bytes:
                break
            if iid == keep:
                continue
            log.info("conversion cache full: dropping copy of %s", meta.get("source"))
            self._remove(iid)
            total -= meta["size"]


def _unlink(path: str) -> None:
    try:
        os.remove(path)
    except FileNotFoundError:
        pass
