package store

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/legacy holds databases written by the Python version (its
// db.py schema, through Python's sqlite3): state.db with newer rows only in
// an un-checkpointed state.db-wal and enough rows for interior b-tree pages,
// and library.db created before the skips/missing/anilist_id columns and
// then ALTERed, with a streams value large enough for overflow pages.

func copyLegacy(t *testing.T, dst string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		in, err := os.Open(filepath.Join("testdata", "legacy", n))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(dst, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
}

func open(t *testing.T, cache, config string) *Store {
	t.Helper()
	s, err := Open(cache, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIDsMatchThePythonVersion(t *testing.T) {
	// hashlib.sha1("Shows\0Show/Show - S01E01.mkv".encode()).hexdigest()[:16] etc.
	if got := ItemID("Shows", "Show/Show - S01E01.mkv"); got != "21690067eda88d6b" {
		t.Error(got)
	}
	if got := SeriesID("Shows", "Show (2020)"); got != "b30e9470f7d78507" {
		t.Error(got)
	}
}

func TestImportsPythonDatabases(t *testing.T) {
	dir := t.TempDir()
	cache, config := filepath.Join(dir, "cache"), filepath.Join(dir, "config")
	copyLegacy(t, cache, "library.db")
	copyLegacy(t, config, "state.db", "state.db-wal")
	s := open(t, cache, config)

	s.View(func(d *Data) {
		if len(d.Progress) != 302 {
			t.Fatalf("%d progress rows", len(d.Progress))
		}
		a := d.Progress["aaaaaaaaaaaaaaaa"] // updated in the WAL only
		if a.Position != 900.25 || *a.Duration != 6000 || a.Watched || a.UpdatedAt != 1700000100 {
			t.Errorf("%+v", a)
		}
		if b := d.Progress["bbbbbbbbbbbbbbbb"]; !b.Watched || b.Position != 0 {
			t.Errorf("%+v", b)
		}
		if c := d.Progress["c000000000000299"]; c.Position != 299 || c.Duration != nil {
			t.Errorf("%+v", c)
		}

		ep := d.Items["1111111111111111"]
		if ep == nil || ep.SeriesID != "5555555555555555" || *ep.Season != 1 || *ep.Duration != 1440.5 ||
			ep.Probed != Probed || len(ep.Streams) != 60 || ep.Streams[59].Index != 59 || *ep.Streams[0].Height != 1080 {
			t.Fatalf("%+v", ep)
		}
		mv := d.Items["2222222222222222"]
		if mv.Kind != "movie" || *mv.Year != 1995 || mv.Probed != ProbeFailed || *mv.ProbeError != "bad file" || mv.SeriesID != "" {
			t.Errorf("%+v", mv)
		}
		if se := d.Series["5555555555555555"]; se.Title != "Show" || se.Missing || *se.Year != 2020 {
			t.Errorf("%+v", se)
		}
		if se := d.Series["6666666666666666"]; !se.Missing || se.Year != nil {
			t.Errorf("%+v", se)
		}
		m := d.Meta["2222222222222222"]
		if *m.TMDBID != 949 || m.AnilistID != nil || *m.Overview != "A heist." || m.ArtAt != 1700000300 {
			t.Errorf("%+v", m)
		}
	})
	s.Close()

	// Imported once: the JSON files are what counts from now on.
	for _, p := range []string{filepath.Join(cache, "library.json"), filepath.Join(config, "progress.jsonl")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(config, "state.db-wal"))
	s2 := open(t, cache, config)
	s2.View(func(d *Data) {
		if d.Progress["aaaaaaaaaaaaaaaa"].Position != 900.25 || len(d.Items) != 2 {
			t.Error("not reloaded from JSON")
		}
	})
}

func TestProgressLogLastLineWins(t *testing.T) {
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "c"), filepath.Join(dir, "s"))
	for i := 0; i < 5; i++ {
		if err := s.SetProgress(Progress{ID: "x", Position: float64(i), UpdatedAt: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	g := s.Generation()
	s.SetProgress(Progress{ID: "y", Watched: true})
	if s.Generation() <= g {
		t.Error("generation did not move")
	}
	s.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "s", "progress.jsonl"))
	if n := strings.Count(string(raw), "\n"); n != 6 {
		t.Errorf("%d lines", n)
	}
	// A crash mid-line leaves a broken tail; it is skipped.
	f, _ := os.OpenFile(filepath.Join(dir, "s", "progress.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":"x","posi`)
	f.Close()

	// A new cache dir: progress does not live in the cache.
	s2 := open(t, filepath.Join(dir, "c2"), filepath.Join(dir, "s"))
	s2.View(func(d *Data) {
		if d.Progress["x"].Position != 4 || !d.Progress["y"].Watched {
			t.Errorf("%+v %+v", d.Progress["x"], d.Progress["y"])
		}
	})
	s2.Close()
	raw, _ = os.ReadFile(filepath.Join(dir, "s", "progress.jsonl"))
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Errorf("not compacted: %d lines", n)
	}
}

func TestLibraryIsWrittenAndReloaded(t *testing.T) {
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "c"), filepath.Join(dir, "s"))
	s.Update(func(d *Data) {
		d.Items["i"] = &Item{ID: "i", Title: "T", Probed: Probed}
		d.Series["s"] = &Series{ID: "s", Title: "S"}
	})
	s.Close()
	s2 := open(t, filepath.Join(dir, "c"), filepath.Join(dir, "s"))
	if it, ok := s2.GetItem("i"); !ok || it.Title != "T" {
		t.Error(it)
	}
	// A broken cache file is rebuilt, not fatal.
	s2.Close()
	os.WriteFile(filepath.Join(dir, "c", "library.json"), []byte("{"), 0o644)
	s3 := open(t, filepath.Join(dir, "c"), filepath.Join(dir, "s"))
	if _, ok := s3.GetItem("i"); ok {
		t.Error("expected an empty library")
	}
}

func TestFailedProgressImportIsRetried(t *testing.T) {
	dir := t.TempDir()
	cache, config := filepath.Join(dir, "cache"), filepath.Join(dir, "config")
	good, err := os.ReadFile(filepath.Join("testdata", "legacy", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(config, 0o755)
	legacy := filepath.Join(config, "state.db")
	os.WriteFile(legacy, good[:200], 0o644) // cut off: the import fails
	s := open(t, cache, config)
	if err := s.SetProgress(Progress{ID: "new", Position: 42, UpdatedAt: 1800000000}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Fixed by hand: the next start imports it, keeping what was watched since.
	os.WriteFile(legacy, good, 0o644)
	s2 := open(t, cache, config)
	s2.View(func(d *Data) {
		if len(d.Progress) < 2 || d.Progress["new"] == nil || d.Progress["new"].Position != 42 {
			t.Errorf("%d rows, new=%+v", len(d.Progress), d.Progress["new"])
		}
	})
	s2.Close()
	if _, err := os.Stat(legacy + ".imported"); err != nil {
		t.Error(err)
	}
}

func TestMalformedSQLiteIsAnErrorNotAPanic(t *testing.T) {
	good, err := os.ReadFile(filepath.Join("testdata", "legacy", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]func(b []byte){
		"cell count": func(b []byte) { b[103], b[104] = 0xff, 0xff },
		"cell pointer": func(b []byte) {
			b[108], b[109] = 0xff, 0xfe
		},
	} {
		b := append([]byte(nil), good...)
		damage(b)
		p := filepath.Join(t.TempDir(), "state.db")
		os.WriteFile(p, b, 0o644)
		if _, err := importProgress(p, map[string]*Progress{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
