"""ffprobe wrapper. Keeps only what playback needs."""

from __future__ import annotations

import json
import subprocess


def probe(ffprobe: str, path: str, timeout: float = 60) -> dict:
    out = subprocess.run(
        [
            ffprobe, "-v", "error", "-hide_banner",
            "-print_format", "json", "-show_format", "-show_streams", path,
        ],
        capture_output=True,
        timeout=timeout,
        check=False,
    )
    if out.returncode != 0:
        raise RuntimeError(out.stderr.decode(errors="replace").strip()[-500:] or "ffprobe failed")
    return summarize(json.loads(out.stdout or b"{}"))


def _float(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def summarize(raw: dict) -> dict:
    """Reduce ffprobe output to the fields we store."""
    fmt = raw.get("format", {})
    streams = []
    video = None
    for s in raw.get("streams", []):
        typ = s.get("codec_type")
        if typ not in ("video", "audio"):
            continue  # subtitles are never shown; attachments/data are useless here
        disp = s.get("disposition") or {}
        if typ == "video" and disp.get("attached_pic"):
            continue  # cover art embedded as a video stream
        tags = {k.lower(): v for k, v in (s.get("tags") or {}).items()}
        entry = {
            "index": s.get("index"),
            "type": typ,
            "codec": (s.get("codec_name") or "").lower(),
            "profile": s.get("profile"),
            "language": (tags.get("language") or "").lower(),
            "title": tags.get("title") or "",
            "default": bool(disp.get("default")),
            "comment": bool(disp.get("comment")) or "commentary" in (tags.get("title") or "").lower(),
        }
        if typ == "video":
            entry.update(
                width=s.get("width"),
                height=s.get("height"),
                pix_fmt=s.get("pix_fmt"),
                color_transfer=s.get("color_transfer"),
            )
            if video is None:
                video = entry
        else:
            entry.update(channels=s.get("channels"), layout=s.get("channel_layout"))
        streams.append(entry)

    return {
        "duration": _float(fmt.get("duration")),
        "start_time": _float(fmt.get("start_time")) or 0.0,
        "container": fmt.get("format_name") or "",
        "video_codec": video["codec"] if video else None,
        "width": video["width"] if video else None,
        "height": video["height"] if video else None,
        "pix_fmt": video["pix_fmt"] if video else None,
        "streams": streams,
    }


def keyframe_before(ffprobe: str, path: str, t: float, start_time: float = 0.0,
                    timeout: float = 8) -> float | None:
    """Time (relative to file start) of the last video keyframe at or before `t`.

    Used when the video stream is copied: ffmpeg can only start a copied
    stream on a keyframe, so we start exactly there and tell the player the
    real start time instead of letting the clock drift by up to a GOP.
    """
    if t <= 0:
        return 0.0
    abs_t = t + start_time
    lo = max(0.0, abs_t - 30)
    try:
        out = subprocess.run(
            [
                ffprobe, "-v", "error", "-select_streams", "v:0", "-skip_frame", "nokey",
                "-show_entries", "frame=pts_time,best_effort_timestamp_time",
                "-of", "csv=p=0", "-read_intervals", f"{lo:.3f}%{abs_t + 0.05:.3f}", path,
            ],
            capture_output=True,
            timeout=timeout,
            check=False,
        )
    except subprocess.TimeoutExpired:
        return None
    best = None
    for line in out.stdout.decode(errors="replace").splitlines():
        for cell in line.split(","):
            v = _float(cell.strip())
            if v is not None and v <= abs_t + 0.05 and (best is None or v > best):
                best = v
    if best is None:
        return None
    return max(0.0, best - start_time)
