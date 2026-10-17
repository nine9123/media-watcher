"""Filename / folder parsing: turn paths into movies, series, seasons and episodes.

Pure functions only, no filesystem access, so everything here is unit-testable.
The rules follow the Jellyfin/Kodi naming conventions plus the usual scene and
fansub patterns:

    Show (2020)/Season 01/Show - S01E02 - Title.mkv
    Show/Show.S01E02.1080p.WEB-DL.x264-GRP.mkv
    [Group] Show - 05 [1080p][ABCD1234].mkv
    Show/Season 2/05 - Title.mkv
    Movie (2010)/Movie (2010) - 1080p.mkv
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass

VIDEO_EXTENSIONS = {
    ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".mts",
    ".webm", ".flv", ".mpg", ".mpeg", ".ogv", ".3gp", ".divx", ".vob",
}

# Directories that are never part of the library. NAS vendors litter shares
# with these (Synology @eaDir, QNAP .@__thumb, recycle bins, ...).
IGNORED_DIRS = {
    "@eadir", ".@__thumb", "#recycle", "#snapshot", "@recycle", ".recycle",
    "$recycle.bin", "lost+found", ".streams", "extras", "extra", "featurettes",
    "behind the scenes", "deleted scenes", "interviews", "scenes", "shorts",
    "trailers", "trailer", "samples", "sample", "other", "bonus", "nc", "ncop",
    "nced", "menus", "scans", "cds", "sps",
}

# Jellyfin-style extras suffixes: "Movie-trailer.mkv", "Movie - sample.mkv"
_EXTRA_SUFFIX = re.compile(
    r"[ ._-](trailer|sample|featurette|behindthescenes|deleted|deletedscene|"
    r"interview|scene|short|extra|clip|other)\d*$",
    re.I,
)
_SAMPLE_WORD = re.compile(r"(^|[ ._\-\[(])sample([ ._\-\])]|$)", re.I)

# Tokens that mark the start of release noise. Everything from the first one
# onward is dropped from a title.
_RELEASE_TOKENS = (
    r"2160p|1080p|1080i|720p|576p|480p|4k|uhd|hdr10\+?|hdr|dv|dovi|"
    r"bluray|blu-ray|bdrip|brrip|bdremux|bd|remux|web-?dl|web-?rip|webrip|web|"
    r"hdtv|dvdrip|dvd|hdrip|amzn|nf|dsnp|hmax|atvp|cr|"
    r"x264|x265|h\.?264|h\.?265|hevc|avc|av1|xvid|divx|10bit|10-bit|8bit|hi10p?|"
    r"aac(?:2\.0|5\.1)?|ac3|eac3|e-ac-3|dts(?:-hd)?|truehd|atmos|flac|opus|ddp?5\.1|"
    r"dual[ .-]?audio|multi[ .-]?audio|multi|multisubs?|proper|repack|"
    r"uncensored|uncut|extended|internal|limited|complete|batch"
)
_RELEASE_RE = re.compile(rf"(?:^|[\s._\-\[(])(?:{_RELEASE_TOKENS})(?=$|[\s._\-\])])", re.I)

_YEAR_PAREN = re.compile(r"[(\[]((?:19|20)\d{2})[)\]]")
_YEAR_BARE = re.compile(r"(?:^|[\s.])((?:19|20)\d{2})(?=$|[\s.])")
_BRACKETS = re.compile(r"\[[^\]]*\]|\{[^}]*\}")
_EMPTY_PARENS = re.compile(r"\(\s*\)")
_SPACES = re.compile(r"\s+")

_SXXEYY = re.compile(
    r"(?<![a-z0-9])s(\d{1,2})[ ._-]{0,3}e(\d{1,4})(?:(?:[-_ ]?e|-)(\d{1,4}))?(?![0-9])",
    re.I,
)
_NXNN = re.compile(r"(?<![a-z0-9])(\d{1,2})x(\d{2,3})(?:-(\d{2,3}))?(?![0-9a-z])", re.I)
_EPISODE_WORD = re.compile(
    r"(?<![a-z])(?:episode|ep\.?)[ ._-]?(\d{1,4})(?![0-9])", re.I
)
# A lone episode marker without a season: "Show.E01.1080p", "Show E12"
_E_NUMBER = re.compile(r"(?<![a-z0-9])e(\d{1,4})(?:v\d)?(?![0-9a-z])", re.I)
# "Show - 05", "Show - 05v2", "Show - 05 - Title", "Show - 05 [1080p]"
_DASH_NUMBER = re.compile(r"^(.*?)\s+-\s+(\d{1,4})(?:v\d)?(?=$|[\s\[(.\-])")
# Leading number inside a season/series folder: "05.mkv", "05 - Title.mkv"
_LEADING_NUMBER = re.compile(r"^(?:e|ep)?(\d{1,4})(?:v\d)?(?=$|[\s._\-\[(])", re.I)
# Trailing number: "Show 05", "Show_05v2"
_TRAILING_NUMBER = re.compile(r"^(.*?)[\s._](\d{1,3})(?:v\d)?$")
# Number followed by a title: "Naruto 001 - Enter Naruto"
_NUMBER_DASH = re.compile(r"^(.*?\S)\s+(\d{1,4})(?:v\d)?\s+-\s+")

_SEASON_DIR = re.compile(
    r"^(?:season|staffel|saison|temporada|series)[ ._-]*(\d{1,3})$|^s(\d{1,3})$", re.I
)
_SPECIALS_DIR = re.compile(r"^(?:specials?|season[ ._-]*0+|s0+|ova|ovas|oad)$", re.I)
_SEASON_IN_NAME = re.compile(
    r"(?:^|[\s._\-])(?:s(\d{1,2})|season[ ._-]?(\d{1,2}))(?=$|[\s._\-\[(])", re.I
)


@dataclass
class ParsedMedia:
    kind: str  # "episode" | "movie"
    title: str  # movie title or episode title (may be "")
    year: int | None = None
    series_title: str | None = None
    series_year: int | None = None
    series_dir: str | None = None  # relative path of the series folder, "" for loose files
    season: int | None = None
    episode: int | None = None
    episode_end: int | None = None


def is_video(name: str) -> bool:
    return os.path.splitext(name)[1].lower() in VIDEO_EXTENSIONS


def is_ignored_dir(name: str) -> bool:
    return name.startswith(".") or name.lower() in IGNORED_DIRS


def is_extra(filename: str) -> bool:
    """Trailers, samples and other bonus clips that should not be listed."""
    stem = os.path.splitext(filename)[0]
    return bool(_EXTRA_SUFFIX.search(stem) or _SAMPLE_WORD.search(stem))


def _is_year(n: int) -> bool:
    return 1900 <= n <= 2099


def clean_title(raw: str, allow_empty: bool = False) -> tuple[str, int | None]:
    """Strip release noise from a name. Returns (title, year).

    With `allow_empty`, a name that is nothing but release noise becomes "".
    """
    s = raw
    if " " not in s and s.count(".") >= 1:
        s = s.replace(".", " ")
    s = s.replace("_", " ")
    s = _BRACKETS.sub(" ", s)

    year = None
    m = _YEAR_PAREN.search(s)
    if m:
        year = int(m.group(1))
        s = s[: m.start()] + " " + s[m.end() :]

    m = _RELEASE_RE.search(s)
    if m and (m.start() > 0 or allow_empty):
        s = s[: m.start()]

    if year is None:
        # A bare year like "Blade Runner 2049 2017" -> take the last year,
        # but never consume the whole title ("1917", "2012").
        years = list(_YEAR_BARE.finditer(s))
        if years:
            last = years[-1]
            before = s[: last.start()].strip(" -.")
            if before:
                year = int(last.group(1))
                s = before + s[last.end() :]

    s = _EMPTY_PARENS.sub(" ", s)
    s = _SPACES.sub(" ", s).strip(" -._,")
    # Unbalanced trailing "(" left over from a cut.
    s = re.sub(r"[(\[]\s*$", "", s).strip(" -._,")
    return s, year


def season_from_dir(name: str) -> int | None:
    """Season number if `name` is a pure season folder ("Season 01", "S2", "Specials")."""
    n = name.strip()
    if _SPECIALS_DIR.match(n):
        return 0
    m = _SEASON_DIR.match(n)
    if m:
        return int(m.group(1) or m.group(2))
    return None


def split_season_in_name(name: str) -> tuple[str, int | None]:
    """'Show.S02.1080p.BluRay-GRP' -> ('Show', 2). Leaves names without a season alone."""
    m = _SEASON_IN_NAME.search(name)
    if not m:
        return name, None
    # Must not be an SxxEyy episode marker.
    if _SXXEYY.search(name[m.start() :]) and _SXXEYY.search(name[m.start() :]).start() <= 1:
        return name, None
    return name[: m.start()], int(m.group(1) or m.group(2))


def season_in_dir(name: str) -> int | None:
    """Season named by a folder, pure ("Season 2") or not ("Show.S02.1080p", "Season 1 (2019)")."""
    season = season_from_dir(name)
    if season is None:
        season = split_season_in_name(name)[1]
    return season


def _episode_title(rest: str) -> str:
    rest = rest.strip()
    if " " not in rest and "." in rest:
        rest = rest.replace(".", " ")
    rest = rest.replace("_", " ")
    rest = rest.lstrip(" -.")
    title, _ = clean_title(rest, allow_empty=True)
    return title


def _match_episode(stem: str):
    """Find an episode marker. Returns (prefix, season, ep, ep_end, rest) or None."""
    m = _SXXEYY.search(stem)
    if m:
        end = int(m.group(3)) if m.group(3) else None
        return stem[: m.start()], int(m.group(1)), int(m.group(2)), end, stem[m.end() :]
    m = _NXNN.search(stem)
    if m and not _is_year(int(m.group(1) + m.group(2))):
        end = int(m.group(3)) if m.group(3) else None
        return stem[: m.start()], int(m.group(1)), int(m.group(2)), end, stem[m.end() :]
    m = _EPISODE_WORD.search(stem)
    if m:
        return stem[: m.start()], None, int(m.group(1)), None, stem[m.end() :]
    m = _E_NUMBER.search(stem)
    if m and m.start() > 0:
        return stem[: m.start()], None, int(m.group(1)), None, stem[m.end() :]
    # Fansub style. Strip leading [Group] first so it does not end up in the title.
    # Underscores stand in for spaces: "[Grp]_Show_-_01_[720p]".
    lead = re.match(r"^(?:\[[^\]]*\][\s_]*)+", stem)
    body = stem[lead.end() :] if lead else stem
    if " " not in body:
        body = body.replace("_", " ")
    m = _DASH_NUMBER.match(body)
    if m and not _is_year(int(m.group(2))):
        return m.group(1), None, int(m.group(2)), None, body[m.end() :]
    return None


def parse_path(rel_path: str, library_type: str = "auto", siblings: int = 1) -> ParsedMedia:
    """Parse a path relative to the library root.

    `library_type` is "auto", "shows" or "movies". `siblings` is the number of
    video files in the same directory; it gates the weakest heuristics (bare
    numbers) so a lone "Movie 2.mkv" is not turned into an episode.
    """
    rel_path = rel_path.replace("\\", "/").strip("/")
    parts = rel_path.split("/")
    filename = parts[-1]
    dirs = parts[:-1]
    stem = os.path.splitext(filename)[0]

    if library_type != "movies":
        ep = _parse_episode(stem, dirs, library_type, siblings)
        if ep is not None:
            return ep
    return _parse_movie(stem, dirs)


def _series_from_dirs(dirs: list[str]):
    """Work out (series_title, series_year, series_dir, season) from the folder chain.

    The series is the top-level folder inside the library, however deep the
    file sits below it ("Show/Show S02/Disc 1/x.mkv" belongs to "Show"), so
    every episode under one show folder lands in one series. The season comes
    from the deepest folder that names one ("Season 2", "Show.S02.1080p").
    """
    if not dirs:
        return None, None, "", None
    season = None
    for d in reversed(dirs):
        season = season_in_dir(d)
        if season is not None:
            break
    if season_from_dir(dirs[0]) is not None:
        # The library itself is one show ("Season 1/x.mkv"): no series folder.
        return None, None, "", season
    name = dirs[0]
    stripped, _ = split_season_in_name(name)
    title, year = clean_title(stripped)
    if not title:
        title, year = clean_title(name)
    return title, year, name, season


def series_from_dir(name: str) -> tuple[str | None, int | None]:
    """(title, year) of the series a top-level library folder would hold;
    title is None when the folder is a season folder, not a show."""
    title, year, _, _ = _series_from_dirs([name])
    return title, year


def _parse_episode(stem: str, dirs: list[str], library_type: str, siblings: int):
    found = _match_episode(stem)
    if found is None and len(dirs) >= 2:
        # Per-episode folder with an obfuscated file inside:
        # "Show/Show.S01E02.1080p-GRP/abc123.mkv", "Show/Episode 01/video.mkv".
        d = _match_episode(dirs[-1])
        if d is not None:
            found = ("", d[1], d[2], d[3], "")
    # Any folder naming a season ("Season 1 (2019)", "Show.S01.1080p-GRP")
    # means everything below it is an episode, numbered or not.
    under_season = any(season_in_dir(d) is not None for d in dirs)

    if found is None:
        # Bare numbers only when context says this is a series.
        if dirs and (under_season or siblings >= 2 or library_type == "shows"):
            m = _LEADING_NUMBER.match(stem)
            if m and not _is_year(int(m.group(1))):
                found = ("", None, int(m.group(1)), None, stem[m.end() :])
            else:
                m = _NUMBER_DASH.match(stem)
                if m and not _is_year(int(m.group(2))):
                    found = (m.group(1), None, int(m.group(2)), None, stem[m.end() :])
            if found is None:
                cleaned = _BRACKETS.sub(" ", stem).strip()
                cut = _RELEASE_RE.search(cleaned)
                if cut and cut.start() > 0:
                    cleaned = cleaned[: cut.start()]
                m = _TRAILING_NUMBER.match(cleaned.strip(" ._-"))
                if m and not _is_year(int(m.group(2))):
                    found = (m.group(1), None, int(m.group(2)), None, "")
        if found is None and library_type != "shows" and not under_season:
            return None
        if found is None:
            found = (stem, None, None, None, "")

    prefix, season, episode, episode_end, rest = found
    name_season = None
    if season is None:
        # "[Grp] Show S2 - 05": the season sits in front of the bare number.
        stripped, name_season = split_season_in_name(prefix)
        if name_season is not None and clean_title(stripped)[0]:
            prefix = stripped
        else:
            name_season = None
    s_title, s_year, s_dir, dir_season = _series_from_dirs(dirs)
    if s_title is None:
        s_title, s_year = clean_title(prefix)
        s_dir = ""
        if not s_title:
            s_title = "Unknown"
    if season is None:
        season = next((s for s in (dir_season, name_season) if s is not None), 1)
    title = _episode_title(rest) if rest else ""
    if episode is None:
        title = clean_title(stem)[0]
    return ParsedMedia(
        kind="episode",
        title=title,
        series_title=s_title,
        series_year=s_year,
        series_dir=s_dir,
        season=season,
        episode=episode,
        episode_end=episode_end,
    )


_GENERIC_STEMS = re.compile(r"^(movie|film|video|main|feature|title[ _]?t?\d*)$", re.I)


def _parse_movie(stem: str, dirs: list[str]) -> ParsedMedia:
    title, year = clean_title(stem)
    if dirs:
        d_title, d_year = clean_title(dirs[-1])
        if not title or _GENERIC_STEMS.match(title):
            title = d_title or title
        if year is None:
            year = d_year
    if not title:
        title = stem
    return ParsedMedia(kind="movie", title=title, year=year)


def sort_key(title: str) -> str:
    t = title.lower()
    for article in ("the ", "a ", "an "):
        if t.startswith(article):
            t = t[len(article) :]
            break
    return t


def title_key(title: str) -> str:
    """Normalised key used to merge files belonging to the same series."""
    return re.sub(r"[^0-9a-zÀ-￿]+", "", title.lower())
