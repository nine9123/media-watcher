package store

import (
	"encoding/json"
	"fmt"

	"github.com/nine9123/media-watcher/internal/probe"
	"github.com/nine9123/media-watcher/internal/sqlite"
)

// Import of the SQLite databases written by the Python version.

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func strp(v any) *string {
	if v == nil {
		return nil
	}
	s := str(v)
	return &s
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func f64(v any) float64 {
	f, _ := num(v)
	return f
}

func f64p(v any) *float64 {
	if f, ok := num(v); ok {
		return &f
	}
	return nil
}

func intp(v any) *int {
	if f, ok := num(v); ok {
		n := int(f)
		return &n
	}
	return nil
}

func importProgress(path string, into map[string]*Progress) (int, error) {
	db, err := sqlite.Open(path)
	if err != nil {
		return 0, err
	}
	rows, err := db.Table("progress")
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		id := str(r["id"])
		if id == "" {
			continue
		}
		into[id] = &Progress{ID: id, Position: f64(r["position"]), Duration: f64p(r["duration"]),
			Watched: f64(r["watched"]) != 0, UpdatedAt: f64(r["updated_at"])}
	}
	return len(rows), nil
}

func importLibrary(path string, d *Data) error {
	db, err := sqlite.Open(path)
	if err != nil {
		return err
	}
	items, err := db.Table("items")
	if err != nil {
		return err
	}
	series, err := db.Table("series")
	if err != nil {
		return err
	}
	meta, err := db.Table("metadata")
	if err != nil {
		return err
	}
	for _, r := range items {
		it := &Item{
			ID: str(r["id"]), Library: str(r["library"]), RelPath: str(r["rel_path"]), Path: str(r["path"]),
			Size: int64(f64(r["size"])), Mtime: f64(r["mtime"]), Kind: str(r["kind"]),
			SeriesID: str(r["series_id"]), Season: intp(r["season"]), Episode: intp(r["episode"]),
			EpisodeEnd: intp(r["episode_end"]), Title: str(r["title"]), Year: intp(r["year"]),
			SortName: str(r["sort_name"]), AddedAt: f64(r["added_at"]), Image: str(r["image"]),
			Probed: int(f64(r["probed"])), ProbeError: strp(r["probe_error"]),
			Duration: f64p(r["duration"]), StartTime: f64p(r["start_time"]), Container: str(r["container"]),
			VideoCodec: strp(r["video_codec"]), Width: intp(r["width"]), Height: intp(r["height"]),
			PixFmt: strp(r["pix_fmt"]),
		}
		if raw := str(r["streams"]); raw != "" {
			var s []probe.Stream
			if json.Unmarshal([]byte(raw), &s) == nil {
				it.Streams = s
			}
		}
		if _, ok := r["skips"]; !ok && it.Probed == Probed {
			// From before intro/credits chapters were read: probe again.
			it.Probed = Unprobed
		} else if raw := str(r["skips"]); raw != "" {
			var s []probe.Skip
			if json.Unmarshal([]byte(raw), &s) == nil {
				it.Skips = s
			}
		}
		if it.Probed == Probed && it.Streams == nil {
			it.Probed = Unprobed
		}
		if it.ID != "" {
			d.Items[it.ID] = it
		}
	}
	for _, r := range series {
		se := &Series{ID: str(r["id"]), Library: str(r["library"]), Title: str(r["title"]),
			Year: intp(r["year"]), Dir: str(r["dir"]), Poster: str(r["poster"]),
			SortName: str(r["sort_name"]), AddedAt: f64(r["added_at"]), Missing: f64(r["missing"]) != 0}
		if se.ID != "" {
			d.Series[se.ID] = se
		}
	}
	for _, r := range meta {
		m := &Meta{ID: str(r["id"]), Kind: str(r["kind"]), Query: str(r["query"]),
			TMDBID: intp(r["tmdb_id"]), AnilistID: intp(r["anilist_id"]), Title: strp(r["title"]),
			Year: intp(r["year"]), Overview: strp(r["overview"]), PosterURL: strp(r["poster_url"]),
			ArtAt: f64(r["art_at"]), FetchedAt: f64(r["fetched_at"])}
		if m.ID != "" {
			d.Meta[m.ID] = m
		}
	}
	return nil
}
