import pytest

from mediawatcher.naming import (
    clean_title,
    is_extra,
    is_ignored_dir,
    parse_path,
    season_from_dir,
    title_key,
)


@pytest.mark.parametrize(
    "raw,title,year",
    [
        ("The.Matrix.1999.1080p.BluRay.x264-GRP", "The Matrix", 1999),
        ("The Matrix (1999)", "The Matrix", 1999),
        ("Blade Runner 2049 (2017)", "Blade Runner 2049", 2017),
        ("1917", "1917", None),
        ("2012 (2009)", "2012", 2009),
        ("[SubsPlease] Frieren [1080p]", "Frieren", None),
        ("Some_Show_720p_x265", "Some Show", None),
        ("Mr. Robot", "Mr. Robot", None),
        ("Show (2020) [WEB-DL 1080p]", "Show", 2020),
    ],
)
def test_clean_title(raw, title, year):
    assert clean_title(raw) == (title, year)


@pytest.mark.parametrize(
    "name,season",
    [
        ("Season 01", 1),
        ("Season 1", 1),
        ("season.2", 2),
        ("S03", 3),
        ("Staffel 4", 4),
        ("Specials", 0),
        ("Season 00", 0),
        ("Breaking Bad", None),
        ("Show S01", None),
    ],
)
def test_season_from_dir(name, season):
    assert season_from_dir(name) == season


def ep(p, **kw):
    r = parse_path(p, **kw)
    assert r.kind == "episode", r
    return r


def test_jellyfin_layout():
    r = ep("Breaking Bad (2008)/Season 01/Breaking Bad - S01E02 - Cat's in the Bag.mkv")
    assert (r.series_title, r.series_year, r.season, r.episode) == ("Breaking Bad", 2008, 1, 2)
    assert r.title == "Cat's in the Bag"
    assert r.series_dir == "Breaking Bad (2008)"


def test_scene_release_in_series_folder():
    r = ep("Show/Show.S02E10.1080p.WEB-DL.x264-GRP.mkv")
    assert (r.series_title, r.season, r.episode, r.title) == ("Show", 2, 10, "")


def test_scene_release_folder_with_season():
    r = ep("Show.Name.S02.1080p.BluRay.x264-GRP/Show.Name.S02E03.1080p.BluRay.x264-GRP.mkv")
    assert (r.series_title, r.season, r.episode) == ("Show Name", 2, 3)


def test_multi_episode():
    r = ep("Show/Season 1/Show - S01E01-E02.mkv")
    assert (r.episode, r.episode_end) == (1, 2)
    r = ep("Show/Season 1/Show S01E03E04.mkv")
    assert (r.episode, r.episode_end) == (3, 4)


def test_fansub_style():
    r = ep("Frieren/[SubsPlease] Sousou no Frieren - 05 (1080p) [ABCD1234].mkv")
    assert (r.series_title, r.season, r.episode) == ("Frieren", 1, 5)


def test_fansub_style_loose_file_uses_filename():
    r = ep("[SubsPlease] Sousou no Frieren - 12v2 [1080p].mkv")
    assert (r.series_title, r.episode, r.series_dir) == ("Sousou no Frieren", 12, "")


def test_fansub_with_season_folder():
    r = ep("Mushishi/Season 2/[Group] Mushishi - 03 - Title Here [BD 1080p].mkv")
    assert (r.series_title, r.season, r.episode, r.title) == ("Mushishi", 2, 3, "Title Here")


def test_nxnn():
    r = ep("Show/Show 2x05 Title.avi")
    assert (r.season, r.episode, r.title) == (2, 5, "Title")


def test_episode_word():
    r = ep("Show/Show Episode 7.mp4")
    assert (r.season, r.episode) == (1, 7)


def test_bare_number_in_season_folder():
    r = ep("Show (2019)/Season 3/05 - The Title.mkv")
    assert (r.series_title, r.series_year, r.season, r.episode, r.title) == (
        "Show", 2019, 3, 5, "The Title"
    )


def test_trailing_number_needs_siblings():
    assert parse_path("Some Film/Some Film 2.mkv").kind == "movie"
    r = parse_path("Anime/Anime 02.mkv", siblings=12)
    assert (r.kind, r.series_title, r.episode) == ("episode", "Anime", 2)


def test_specials_folder():
    r = ep("Show/Specials/Show - S00E01.mkv")
    assert r.season == 0
    r = ep("Show/Specials/Show - 01.mkv")
    assert r.season == 0


def test_movies():
    r = parse_path("The Matrix (1999)/The Matrix (1999).mkv")
    assert (r.kind, r.title, r.year) == ("movie", "The Matrix", 1999)
    r = parse_path("Inception.2010.2160p.UHD.BluRay.x265.mkv")
    assert (r.kind, r.title, r.year) == ("movie", "Inception", 2010)
    r = parse_path("Heat (1995)/movie.mkv")
    assert (r.kind, r.title, r.year) == ("movie", "Heat", 1995)
    # Years must not be mistaken for episode numbers.
    r = parse_path("Movies/Something - 2019.mkv", siblings=5)
    assert r.kind == "movie"


def test_resolution_is_not_an_episode():
    r = parse_path("Movie Name - 1080p.mkv")
    assert r.kind == "movie" and r.title == "Movie Name"


def test_movies_library_never_episodes():
    assert parse_path("Show/Show - S01E01.mkv", library_type="movies").kind == "movie"


def test_shows_library_unnumbered():
    r = parse_path("Show/Pilot.mkv", library_type="shows")
    assert (r.kind, r.series_title, r.episode) == ("episode", "Show", None)


def test_extras_and_ignored():
    assert is_extra("Movie-trailer.mkv")
    assert is_extra("movie.sample.mkv")
    assert is_extra("Sample.mkv")
    assert not is_extra("Samplers Guide - S01E01.mkv")
    assert is_ignored_dir("@eaDir")
    assert is_ignored_dir(".@__thumb")
    assert is_ignored_dir("Extras")
    assert not is_ignored_dir("Season 1")


def test_title_key_merges_variants():
    assert title_key("Steins;Gate") == title_key("Steins Gate") == "steinsgate"


def test_nested_folders_belong_to_top_level_series():
    r = ep("Show (2020)/Show S02/Show - S02E03.mkv")
    assert (r.series_title, r.series_year, r.series_dir, r.season) == ("Show", 2020, "Show (2020)", 2)
    r = ep("Show/Season 2/Disc 1/Show - 04.mkv")
    assert (r.series_dir, r.season, r.episode) == ("Show", 2, 4)
    r = ep("Show/Arc Two/Show - 13.mkv")
    assert (r.series_title, r.series_dir, r.season, r.episode) == ("Show", "Show", 1, 13)


def test_episode_from_release_folder_when_file_is_obfuscated():
    r = ep("Show/Show.S01E02.1080p.WEB-DL-GRP/abc123.mkv")
    assert (r.series_dir, r.season, r.episode, r.title) == ("Show", 1, 2, "")


def test_library_that_is_one_show():
    r = ep("Season 2/Show - S02E01.mkv")
    assert (r.series_title, r.series_dir, r.season) == ("Show", "", 2)


@pytest.mark.parametrize(
    "path,siblings,series,season,episode,title",
    [
        # Unnumbered files under a season folder are still episodes.
        ("Show/Season 1/Pilot.mkv", 1, "Show", 1, None, "Pilot"),
        ("Show/Season 2 (2019)/Pilot.mkv", 1, "Show", 2, None, "Pilot"),
        ("Show.S01.1080p-GRP/Pilot.mkv", 1, "Show", 1, None, "Pilot"),
        # Underscores instead of spaces.
        ("Show/[Grp]_Show_-_01_[720p].mkv", 1, "Show", 1, 1, ""),
        ("[Grp]_Show_-_01_[720p].mkv", 1, "Show", 1, 1, ""),
        ("Show/Show_05_[720p].mkv", 3, "Show", 1, 5, ""),
        # Lone E-marker and spaced SxxEyy.
        ("Show/Show.E01.1080p.mkv", 1, "Show", 1, 1, ""),
        ("Show/Show S02 - E03.mkv", 1, "Show", 2, 3, ""),
        # Number then title.
        ("Naruto/Naruto 001 - Enter Naruto.mkv", 5, "Naruto", 1, 1, "Enter Naruto"),
        # Per-episode folders.
        ("Show/Episode 01/video.mkv", 1, "Show", 1, 1, ""),
        ("Show/[Grp] Show - 04 [1080p]/abc.mkv", 1, "Show", 1, 4, ""),
        # Season in front of a fansub number.
        ("[SubsPlease] Show S2 - 05 (1080p).mkv", 1, "Show", 2, 5, ""),
        ("Show/[SubsPlease] Show S2 - 05 (1080p).mkv", 1, "Show", 2, 5, ""),
    ],
)
def test_episodes_in_folders_are_not_movies(path, siblings, series, season, episode, title):
    r = ep(path, siblings=siblings)
    assert (r.series_title, r.season, r.episode, r.title) == (series, season, episode, title)


@pytest.mark.parametrize(
    "path",
    [
        "WALL-E (2008)/WALL-E (2008).mkv",
        "S1m0ne (2002)/S1m0ne (2002).mkv",
        "Movie.2010.1080p.DDP5.1.E-AC3.x264.mkv",
        "Apollo 13 (1995)/Apollo 13.mkv",
    ],
)
def test_new_episode_rules_leave_movies_alone(path):
    assert parse_path(path).kind == "movie"
