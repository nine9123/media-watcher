"""Playback decisions: which audio track, and direct play vs remux vs transcode.

The order of preference is chosen for a small NAS CPU:

  direct     the file is already browser-playable (mp4/webm, playable codecs,
             wanted audio track is the first one). Served with byte ranges,
             zero CPU.
  remux      video is playable but the container is not (mkv, avi, ts...) or
             the wanted audio track is not the first one. Video is copied,
             audio copied or converted to AAC. Costs almost nothing.
  transcode  video codec is not playable in this browser (HEVC on most
             desktop browsers, 10-bit H.264, MPEG-2, VC-1, ...). Only then
             do we encode video.

Subtitles are never mapped, in any mode.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from .config import Config

# Codecs every target browser plays in MP4. Anything else must be advertised
# by the client in its capability list.
BASE_VIDEO = {"h264"}
BASE_AUDIO = {"aac", "mp3"}
H264_OK_PIX = {"yuv420p", "yuvj420p"}
HDR_TRANSFERS = {"smpte2084", "arib-std-b67"}
MP4_EXTS = {".mp4", ".m4v"}


@dataclass
class Caps:
    video: set[str] = field(default_factory=lambda: set(BASE_VIDEO))
    audio: set[str] = field(default_factory=lambda: set(BASE_AUDIO))

    @classmethod
    def parse(cls, video: str | None, audio: str | None) -> "Caps":
        v = {x.strip().lower() for x in (video or "").split(",") if x.strip()}
        a = {x.strip().lower() for x in (audio or "").split(",") if x.strip()}
        return cls(video=v | BASE_VIDEO, audio=a | BASE_AUDIO)


@dataclass
class Decision:
    mode: str  # direct | remux | transcode
    video_index: int | None
    audio_index: int | None
    video: str  # copy | transcode | none
    audio: str  # copy | aac | none
    reason: str = ""


def audio_streams(streams: list[dict]) -> list[dict]:
    return [s for s in streams if s.get("type") == "audio"]


def _lang_match(s: dict, langs: list[str]) -> bool:
    lang = (s.get("language") or "").lower()
    if lang and lang in langs:
        return True
    title = (s.get("title") or "").lower()
    return any(len(l) > 3 and l in title for l in langs)


def pick_audio(streams: list[dict], langs: list[str]) -> dict | None:
    """Preferred language first, then non-commentary, then default flag, then file order."""
    audio = audio_streams(streams)
    if not audio:
        return None
    ranked = sorted(
        enumerate(audio),
        key=lambda p: (
            not _lang_match(p[1], langs),
            bool(p[1].get("comment")),
            not p[1].get("default"),
            p[0],
        ),
    )
    return ranked[0][1]


def video_playable(v: dict | None, caps: Caps) -> bool:
    if v is None:
        return True
    codec = v.get("codec")
    pix = v.get("pix_fmt") or ""
    ten_bit = "10" in pix or "12" in pix
    if codec == "h264":
        profile = (v.get("profile") or "").lower()
        return pix in H264_OK_PIX and "10" not in profile and "4:" not in profile
    if codec == "hevc":
        if "hevc" not in caps.video:
            return False
        return not ten_bit or "hevc10" in caps.video
    if codec == "av1":
        return "av1" in caps.video
    if codec == "vp9":
        return "vp9" in caps.video
    return False


def audio_copyable(a: dict | None, caps: Caps) -> bool:
    return a is None or a.get("codec") in caps.audio


def decide(item: dict, caps: Caps, langs: list[str], audio_index: int | None = None,
           force: str | None = None) -> Decision:
    """`force` is "remux" (never direct) or "transcode" (encode everything).

    The player uses it to step down when the browser fails on a file it
    claimed to support.
    """
    d = _decide(item, caps, langs, audio_index)
    if force == "transcode" and d.video_index is not None:
        return Decision("transcode", d.video_index, d.audio_index, "transcode",
                        "none" if d.audio_index is None else "aac", "forced transcode")
    if force in ("remux", "transcode") and d.mode == "direct":
        return Decision("remux", d.video_index, d.audio_index, d.video, d.audio, "forced remux")
    return d


def _decide(item: dict, caps: Caps, langs: list[str], audio_index: int | None) -> Decision:
    streams = item.get("streams") or []
    video = next((s for s in streams if s.get("type") == "video"), None)
    audios = audio_streams(streams)
    chosen = None
    if audio_index is not None:
        chosen = next((s for s in audios if s.get("index") == audio_index), None)
    if chosen is None:
        chosen = pick_audio(streams, langs)

    v_idx = video["index"] if video else None
    a_idx = chosen["index"] if chosen else None
    v_ok = video_playable(video, caps)
    a_ok = audio_copyable(chosen, caps)
    a_mode = "none" if chosen is None else ("copy" if a_ok else "aac")
    v_mode = "none" if video is None else ("copy" if v_ok else "transcode")

    ext = os.path.splitext(item.get("path") or "")[1].lower()
    container = item.get("container") or ""
    is_mp4 = ext in MP4_EXTS and "mp4" in container
    first_audio = chosen is None or chosen is audios[0]

    if v_mode == "transcode":
        return Decision("transcode", v_idx, a_idx, v_mode, a_mode,
                        f"video {video.get('codec')} {video.get('pix_fmt')} not playable")
    if is_mp4 and v_ok and a_ok and first_audio:
        return Decision("direct", v_idx, a_idx, v_mode, a_mode, "file is browser-playable")
    if ext == ".webm" and first_audio and video and video.get("codec") in ("vp8", "vp9", "av1") \
            and (chosen is None or chosen.get("codec") in ("opus", "vorbis")):
        return Decision("direct", v_idx, a_idx, v_mode, a_mode, "webm is browser-playable")
    reasons = []
    if not is_mp4:
        reasons.append(f"container {ext or container}")
    if not first_audio:
        reasons.append("selected audio is not the first track")
    if not a_ok:
        reasons.append(f"audio {chosen.get('codec')}")
    return Decision("remux", v_idx, a_idx, v_mode, a_mode, ", ".join(reasons) or "remux")


def _audio_args(cfg: Config, a: dict | None, mode: str) -> list[str]:
    if mode == "none":
        return ["-an"]
    if mode == "copy":
        return ["-c:a", "copy"]
    src_ch = (a or {}).get("channels") or 2
    ch = max(1, min(src_ch, cfg.audio_channels))
    bitrate = "192k" if ch <= 2 else "448k"
    return ["-c:a", "aac", "-b:a", bitrate, "-ac", str(ch)]


def _target_height(cfg: Config, height: int | None) -> int | None:
    if height and cfg.max_height and height > cfg.max_height:
        return cfg.max_height
    return None


def ffmpeg_args(cfg: Config, item: dict, d: Decision, start: float = 0.0) -> list[str]:
    """Command line producing a fragmented MP4 on stdout."""
    streams = item.get("streams") or []
    by_index = {s.get("index"): s for s in streams}
    video = by_index.get(d.video_index)
    audio = by_index.get(d.audio_index)
    vaapi = d.video == "transcode" and cfg.hwaccel == "vaapi"

    args = [cfg.ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin"]
    if vaapi:
        args += ["-hwaccel", "vaapi", "-hwaccel_device", cfg.vaapi_device,
                 "-hwaccel_output_format", "vaapi"]
    if start > 0:
        args += ["-ss", f"{start:.3f}"]
    args += ["-i", item["path"]]
    if d.video_index is not None:
        args += ["-map", f"0:{d.video_index}"]
    if d.audio_index is not None:
        args += ["-map", f"0:{d.audio_index}"]
    args += ["-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1"]

    if d.video == "copy":
        args += ["-c:v", "copy"]
        if video and video.get("codec") == "hevc":
            args += ["-tag:v", "hvc1"]  # Safari/Chrome want hvc1, not hev1
    elif d.video == "transcode":
        h = _target_height(cfg, (video or {}).get("height"))
        hdr = (video or {}).get("color_transfer") in HDR_TRANSFERS
        if vaapi:
            scale = f"scale_vaapi=w=-2:h={h}:format=nv12" if h else "scale_vaapi=format=nv12"
            args += ["-vf", scale, "-c:v", "h264_vaapi", "-qp", str(cfg.video_crf + 1)]
        else:
            filters = []
            if h:
                filters.append(f"scale=-2:{h}")
            if hdr:
                filters.append(
                    "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,"
                    "tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"
                )
            filters.append("format=yuv420p")
            args += [
                "-vf", ",".join(filters),
                "-c:v", "libx264", "-preset", cfg.x264_preset, "-crf", str(cfg.video_crf),
                "-maxrate", "20M", "-bufsize", "40M", "-profile:v", "high",
            ]
        # Short GOP so the stream starts quickly and fragments stay small.
        args += ["-g", "48", "-keyint_min", "48", "-sc_threshold", "0"]

    args += _audio_args(cfg, audio, d.audio)
    args += [
        "-avoid_negative_ts", "make_zero",
        "-max_muxing_queue_size", "4096",
        "-movflags", "frag_keyframe+empty_moov+default_base_moof",
        "-frag_duration", "2000000",
        "-f", "mp4", "pipe:1",
    ]
    return args


def describe_audio(streams: list[dict]) -> list[dict]:
    out = []
    for s in audio_streams(streams):
        parts = []
        if s.get("language"):
            parts.append(s["language"].upper())
        if s.get("title"):
            parts.append(s["title"])
        parts.append(f"{s.get('codec', '?')} {s.get('channels') or '?'}ch")
        out.append({"index": s.get("index"), "label": " · ".join(parts),
                    "language": s.get("language") or ""})
    return out
