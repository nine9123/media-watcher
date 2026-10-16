"""Configuration, read once from environment variables."""

from __future__ import annotations

import os
from dataclasses import dataclass, field


@dataclass
class Library:
    name: str
    path: str
    type: str = "auto"  # auto | shows | movies


@dataclass
class Config:
    api_key: str
    libraries: list[Library]
    cache_dir: str = "/cache"
    config_dir: str = "/config"
    host: str = "0.0.0.0"
    port: int = 8097
    scan_interval: int = 1800  # seconds, 0 disables periodic scans
    probe_workers: int = 4
    thumb_workers: int = 2
    audio_languages: list[str] = field(default_factory=lambda: ["jpn", "ja", "japanese"])
    audio_channels: int = 2  # channels when audio has to be transcoded
    hwaccel: str = "none"  # none | vaapi
    vaapi_device: str = "/dev/dri/renderD128"
    max_height: int = 1080  # only applies when video must be transcoded
    video_crf: int = 21
    x264_preset: str = "veryfast"
    ffmpeg: str = "ffmpeg"
    ffprobe: str = "ffprobe"
    web_dir: str = ""


def _libraries_from_env(spec: str, media_root: str) -> list[Library]:
    """LIBRARIES="Anime=/media/anime:shows;Movies=/media/movies:movies".

    Without LIBRARIES every top-level directory of MEDIA_ROOT becomes an
    auto-detected library.
    """
    libs: list[Library] = []
    if spec.strip():
        for entry in spec.split(";"):
            entry = entry.strip()
            if not entry:
                continue
            if "=" not in entry:
                raise ValueError(f"LIBRARIES entry needs Name=/path[:type]: {entry!r}")
            name, rest = entry.split("=", 1)
            path, typ = rest, "auto"
            head, sep, tail = rest.rpartition(":")
            if sep and tail in ("auto", "shows", "movies"):
                path, typ = head, tail
            libs.append(Library(name.strip(), path.strip(), typ))
        return libs
    if os.path.isdir(media_root):
        for entry in sorted(os.scandir(media_root), key=lambda e: e.name.lower()):
            if entry.is_dir() and not entry.name.startswith((".", "@", "#")):
                typ = "movies" if "movie" in entry.name.lower() or "film" in entry.name.lower() else "auto"
                libs.append(Library(entry.name, entry.path, typ))
    return libs


def load(env: dict[str, str] | None = None) -> Config:
    env = dict(os.environ if env is None else env)
    key = env.get("API_KEY", "").strip()
    if len(key) < 8:
        raise SystemExit("API_KEY must be set to at least 8 characters")
    here = os.path.dirname(os.path.abspath(__file__))
    return Config(
        api_key=key,
        libraries=_libraries_from_env(env.get("LIBRARIES", ""), env.get("MEDIA_ROOT", "/media")),
        cache_dir=env.get("CACHE_DIR", "/cache"),
        config_dir=env.get("CONFIG_DIR", "/config"),
        host=env.get("HOST", "0.0.0.0"),
        port=int(env.get("PORT", "8097")),
        scan_interval=int(env.get("SCAN_INTERVAL", "1800")),
        probe_workers=max(1, int(env.get("PROBE_WORKERS", "4"))),
        thumb_workers=max(1, int(env.get("THUMB_WORKERS", "2"))),
        audio_languages=[
            x.strip().lower()
            for x in env.get("AUDIO_LANGUAGES", "jpn,ja,japanese").split(",")
            if x.strip()
        ],
        audio_channels=int(env.get("AUDIO_CHANNELS", "2")),
        hwaccel=env.get("HWACCEL", "none").lower(),
        vaapi_device=env.get("VAAPI_DEVICE", "/dev/dri/renderD128"),
        max_height=int(env.get("MAX_HEIGHT", "1080")),
        video_crf=int(env.get("VIDEO_CRF", "21")),
        x264_preset=env.get("X264_PRESET", "veryfast"),
        ffmpeg=env.get("FFMPEG", "ffmpeg"),
        ffprobe=env.get("FFPROBE", "ffprobe"),
        web_dir=env.get("WEB_DIR", os.path.join(os.path.dirname(here), "web")),
    )
