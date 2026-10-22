package naming

import (
	"fmt"
	"testing"
)

func iv(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		raw, title string
		year       any
	}{
		{"The.Matrix.1999.1080p.BluRay.x264-GRP", "The Matrix", 1999},
		{"The Matrix (1999)", "The Matrix", 1999},
		{"Blade Runner 2049 (2017)", "Blade Runner 2049", 2017},
		{"Blade Runner 2049 2017", "Blade Runner 2049", 2017},
		{"1917", "1917", nil},
		{"2012 (2009)", "2012", 2009},
		{"[SubsPlease] Frieren [1080p]", "Frieren", nil},
		{"Some_Show_720p_x265", "Some Show", nil},
		{"Mr. Robot", "Mr. Robot", nil},
		{"Show (2020) [WEB-DL 1080p]", "Show", 2020},
	}
	for _, c := range cases {
		title, year := CleanTitle(c.raw, false)
		if title != c.title || iv(year) != c.year {
			t.Errorf("CleanTitle(%q) = %q, %v; want %q, %v", c.raw, title, iv(year), c.title, c.year)
		}
	}
}

func TestSeasonFromDir(t *testing.T) {
	cases := map[string]any{
		"Season 01": 1, "Season 1": 1, "season.2": 2, "S03": 3, "Staffel 4": 4,
		"Specials": 0, "Season 00": 0, "Breaking Bad": nil, "Show S01": nil,
	}
	for name, want := range cases {
		if got := iv(SeasonFromDir(name)); got != want {
			t.Errorf("SeasonFromDir(%q) = %v, want %v", name, got, want)
		}
	}
}

func ep(t *testing.T, path, libType string, siblings int) Parsed {
	t.Helper()
	r := ParsePath(path, libType, siblings)
	if r.Kind != "episode" {
		t.Fatalf("%q: kind %q, want episode (%+v)", path, r.Kind, r)
	}
	return r
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

type tup []any

func TestJellyfinLayout(t *testing.T) {
	r := ep(t, "Breaking Bad (2008)/Season 01/Breaking Bad - S01E02 - Cat's in the Bag.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.SeriesYear), iv(r.Season), iv(r.Episode)}, tup{"Breaking Bad", 2008, 1, 2})
	eq(t, r.Title, "Cat's in the Bag")
	eq(t, r.SeriesDir, "Breaking Bad (2008)")
}

func TestSceneRelease(t *testing.T) {
	r := ep(t, "Show/Show.S02E10.1080p.WEB-DL.x264-GRP.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.Season), iv(r.Episode), r.Title}, tup{"Show", 2, 10, ""})
	r = ep(t, "Show.Name.S02.1080p.BluRay.x264-GRP/Show.Name.S02E03.1080p.BluRay.x264-GRP.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.Season), iv(r.Episode)}, tup{"Show Name", 2, 3})
}

func TestMultiEpisode(t *testing.T) {
	r := ep(t, "Show/Season 1/Show - S01E01-E02.mkv", "auto", 1)
	eq(t, tup{iv(r.Episode), iv(r.EpisodeEnd)}, tup{1, 2})
	r = ep(t, "Show/Season 1/Show S01E03E04.mkv", "auto", 1)
	eq(t, tup{iv(r.Episode), iv(r.EpisodeEnd)}, tup{3, 4})
	r = ep(t, "Show/Season 1/Show - S01E05-06 - Title.mkv", "auto", 1)
	eq(t, tup{iv(r.Episode), iv(r.EpisodeEnd), r.Title}, tup{5, 6, "Title"})
	// A resolution after a dash is not the end of a range.
	r = ep(t, "Show/Show.S01E01-1080p.mkv", "auto", 1)
	eq(t, tup{iv(r.Episode), iv(r.EpisodeEnd), r.Title}, tup{1, nil, ""})
	r = ep(t, "Show/Show S01E05-720p.WEB.mkv", "auto", 1)
	eq(t, tup{iv(r.Episode), iv(r.EpisodeEnd), r.Title}, tup{5, nil, ""})
}

func TestFansubStyle(t *testing.T) {
	r := ep(t, "Frieren/[SubsPlease] Sousou no Frieren - 05 (1080p) [ABCD1234].mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.Season), iv(r.Episode)}, tup{"Frieren", 1, 5})
	r = ep(t, "[SubsPlease] Sousou no Frieren - 12v2 [1080p].mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.Episode), r.SeriesDir}, tup{"Sousou no Frieren", 12, ""})
	r = ep(t, "Mushishi/Season 2/[Group] Mushishi - 03 - Title Here [BD 1080p].mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.Season), iv(r.Episode), r.Title}, tup{"Mushishi", 2, 3, "Title Here"})
}

func TestNxNNAndEpisodeWord(t *testing.T) {
	r := ep(t, "Show/Show 2x05 Title.avi", "auto", 1)
	eq(t, tup{iv(r.Season), iv(r.Episode), r.Title}, tup{2, 5, "Title"})
	r = ep(t, "Show/Show Episode 7.mp4", "auto", 1)
	eq(t, tup{iv(r.Season), iv(r.Episode)}, tup{1, 7})
}

func TestBareNumberInSeasonFolder(t *testing.T) {
	r := ep(t, "Show (2019)/Season 3/05 - The Title.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.SeriesYear), iv(r.Season), iv(r.Episode), r.Title},
		tup{"Show", 2019, 3, 5, "The Title"})
}

func TestTrailingNumberNeedsSiblings(t *testing.T) {
	eq(t, ParsePath("Some Film/Some Film 2.mkv", "auto", 1).Kind, "movie")
	r := ParsePath("Anime/Anime 02.mkv", "auto", 12)
	eq(t, tup{r.Kind, r.SeriesTitle, iv(r.Episode)}, tup{"episode", "Anime", 2})
}

func TestSpecialsFolder(t *testing.T) {
	eq(t, iv(ep(t, "Show/Specials/Show - S00E01.mkv", "auto", 1).Season), 0)
	eq(t, iv(ep(t, "Show/Specials/Show - 01.mkv", "auto", 1).Season), 0)
}

func TestMovies(t *testing.T) {
	r := ParsePath("The Matrix (1999)/The Matrix (1999).mkv", "auto", 1)
	eq(t, tup{r.Kind, r.Title, iv(r.Year)}, tup{"movie", "The Matrix", 1999})
	r = ParsePath("Inception.2010.2160p.UHD.BluRay.x265.mkv", "auto", 1)
	eq(t, tup{r.Kind, r.Title, iv(r.Year)}, tup{"movie", "Inception", 2010})
	r = ParsePath("Heat (1995)/movie.mkv", "auto", 1)
	eq(t, tup{r.Kind, r.Title, iv(r.Year)}, tup{"movie", "Heat", 1995})
	// Years must not be mistaken for episode numbers.
	eq(t, ParsePath("Movies/Something - 2019.mkv", "auto", 5).Kind, "movie")
	r = ParsePath("Movie Name - 1080p.mkv", "auto", 1)
	eq(t, tup{r.Kind, r.Title}, tup{"movie", "Movie Name"})
	eq(t, ParsePath("Show/Show - S01E01.mkv", "movies", 1).Kind, "movie")
}

func TestShowsLibraryUnnumbered(t *testing.T) {
	r := ParsePath("Show/Pilot.mkv", "shows", 1)
	eq(t, tup{r.Kind, r.SeriesTitle, iv(r.Episode)}, tup{"episode", "Show", nil})
}

func TestExtrasAndIgnored(t *testing.T) {
	for _, n := range []string{"Movie-trailer.mkv", "movie.sample.mkv", "Sample.mkv"} {
		if !IsExtra(n) {
			t.Errorf("%q should be an extra", n)
		}
	}
	if IsExtra("Samplers Guide - S01E01.mkv") {
		t.Error("Samplers Guide is not an extra")
	}
	for _, d := range []string{"@eaDir", ".@__thumb", "Extras"} {
		if !IsIgnoredDir(d) {
			t.Errorf("%q should be ignored", d)
		}
	}
	if IsIgnoredDir("Season 1") {
		t.Error("Season 1 is not ignored")
	}
}

func TestTitleKey(t *testing.T) {
	eq(t, TitleKey("Steins;Gate"), "steinsgate")
	eq(t, TitleKey("Steins Gate"), "steinsgate")
}

func TestNestedFolders(t *testing.T) {
	r := ep(t, "Show (2020)/Show S02/Show - S02E03.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, iv(r.SeriesYear), r.SeriesDir, iv(r.Season)}, tup{"Show", 2020, "Show (2020)", 2})
	r = ep(t, "Show/Season 2/Disc 1/Show - 04.mkv", "auto", 1)
	eq(t, tup{r.SeriesDir, iv(r.Season), iv(r.Episode)}, tup{"Show", 2, 4})
	r = ep(t, "Show/Arc Two/Show - 13.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, r.SeriesDir, iv(r.Season), iv(r.Episode)}, tup{"Show", "Show", 1, 13})
	r = ep(t, "Show/Show.S01E02.1080p.WEB-DL-GRP/abc123.mkv", "auto", 1)
	eq(t, tup{r.SeriesDir, iv(r.Season), iv(r.Episode), r.Title}, tup{"Show", 1, 2, ""})
	r = ep(t, "Season 2/Show - S02E01.mkv", "auto", 1)
	eq(t, tup{r.SeriesTitle, r.SeriesDir, iv(r.Season)}, tup{"Show", "", 2})
}

func TestEpisodesInFoldersAreNotMovies(t *testing.T) {
	cases := []struct {
		path     string
		siblings int
		series   string
		season   any
		episode  any
		title    string
	}{
		// Unnumbered files under a season folder are still episodes.
		{"Show/Season 1/Pilot.mkv", 1, "Show", 1, nil, "Pilot"},
		{"Show/Season 2 (2019)/Pilot.mkv", 1, "Show", 2, nil, "Pilot"},
		{"Show.S01.1080p-GRP/Pilot.mkv", 1, "Show", 1, nil, "Pilot"},
		// Underscores instead of spaces.
		{"Show/[Grp]_Show_-_01_[720p].mkv", 1, "Show", 1, 1, ""},
		{"[Grp]_Show_-_01_[720p].mkv", 1, "Show", 1, 1, ""},
		{"Show/Show_05_[720p].mkv", 3, "Show", 1, 5, ""},
		// Lone E-marker and spaced SxxEyy.
		{"Show/Show.E01.1080p.mkv", 1, "Show", 1, 1, ""},
		{"Show/Show S02 - E03.mkv", 1, "Show", 2, 3, ""},
		// Number then title.
		{"Naruto/Naruto 001 - Enter Naruto.mkv", 5, "Naruto", 1, 1, "Enter Naruto"},
		// Per-episode folders.
		{"Show/Episode 01/video.mkv", 1, "Show", 1, 1, ""},
		{"Show/[Grp] Show - 04 [1080p]/abc.mkv", 1, "Show", 1, 4, ""},
		// Season in front of a fansub number.
		{"[SubsPlease] Show S2 - 05 (1080p).mkv", 1, "Show", 2, 5, ""},
		{"Show/[SubsPlease] Show S2 - 05 (1080p).mkv", 1, "Show", 2, 5, ""},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			r := ep(t, c.path, "auto", c.siblings)
			eq(t, tup{r.SeriesTitle, iv(r.Season), iv(r.Episode), r.Title}, tup{c.series, c.season, c.episode, c.title})
		})
	}
}

func TestNewEpisodeRulesLeaveMoviesAlone(t *testing.T) {
	for _, p := range []string{
		"WALL-E (2008)/WALL-E (2008).mkv",
		"S1m0ne (2002)/S1m0ne (2002).mkv",
		"Movie.2010.1080p.DDP5.1.E-AC3.x264.mkv",
		"Apollo 13 (1995)/Apollo 13.mkv",
	} {
		if k := ParsePath(p, "auto", 1).Kind; k != "movie" {
			t.Errorf("%q: %s", p, k)
		}
	}
}

func TestSplitExt(t *testing.T) {
	for in, want := range map[string][2]string{
		"a.mkv": {"a", ".mkv"}, ".mkv": {".mkv", ""}, "a.b.mkv": {"a.b", ".mkv"}, "noext": {"noext", ""},
	} {
		s, e := SplitExt(in)
		if s != want[0] || e != want[1] {
			t.Errorf("SplitExt(%q) = %q, %q", in, s, e)
		}
	}
}
