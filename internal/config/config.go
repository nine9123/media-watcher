// Package config reads the configuration once from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Library struct {
	Name string
	Path string
	Type string // auto | shows | movies
}

type Config struct {
	APIKey              string
	Libraries           []Library
	CacheDir            string
	ConfigDir           string
	Host                string
	Port                int
	ScanInterval        int // seconds, 0 disables periodic scans
	ProbeWorkers        int
	ThumbWorkers        int
	AudioLanguages      []string
	AudioChannels       int    // channels when audio has to be transcoded
	HWAccel             string // none | vaapi
	VAAPIDevice         string
	MaxHeight           int // only applies when video must be transcoded
	VideoCRF            int
	X264Preset          string
	TranscodeThreads    int    // libx264 threads; 0 lets x264 pick (1.5 x cores)
	TMDBAPIKey          string // empty: TMDB is skipped
	TMDBLanguage        string
	MetadataRefreshDays int
	// Tried in order, first match wins. Empty: no metadata lookups at all.
	MetadataSources []string
	// Files that need a CPU transcode are converted once in the background and
	// the browser-playable copy is kept in CACHE_DIR/converted.
	AutoConvert      bool
	ConvertCacheDays float64 // a copy not played for this long is deleted
	ConvertCacheGB   float64 // least recently played copies go first
	FFmpeg           string
	FFprobe          string
	WebDir           string // empty: the UI built into the binary
}

// Defaults are the values load() uses when a variable is not set.
func Defaults() Config {
	return Config{
		CacheDir: "/cache", ConfigDir: "/config", Host: "0.0.0.0", Port: 8097,
		ScanInterval: 1800, ProbeWorkers: 4, ThumbWorkers: 2,
		AudioLanguages: []string{"jpn", "ja", "japanese"}, AudioChannels: 2,
		HWAccel: "none", VAAPIDevice: "/dev/dri/renderD128", MaxHeight: 1080, VideoCRF: 21,
		X264Preset: "veryfast", TranscodeThreads: 4, TMDBLanguage: "en-US",
		MetadataRefreshDays: 30, MetadataSources: []string{"tmdb", "anilist"},
		AutoConvert: true, ConvertCacheDays: 14, ConvertCacheGB: 50,
		FFmpeg: "ffmpeg", FFprobe: "ffprobe",
	}
}

// LibrariesFromEnv parses LIBRARIES="Anime=/media/anime:shows;Movies=/media/movies:movies".
//
// Without LIBRARIES every top-level directory of MEDIA_ROOT becomes an
// auto-detected library.
func LibrariesFromEnv(spec, mediaRoot string) ([]Library, error) {
	var libs []Library
	if strings.TrimSpace(spec) != "" {
		for _, entry := range strings.Split(spec, ";") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			name, rest, ok := strings.Cut(entry, "=")
			if !ok {
				return nil, fmt.Errorf("LIBRARIES entry needs Name=/path[:type]: %q", entry)
			}
			path, typ := rest, "auto"
			if i := strings.LastIndex(rest, ":"); i >= 0 {
				switch tail := rest[i+1:]; tail {
				case "auto", "shows", "movies":
					path, typ = rest[:i], tail
				}
			}
			libs = append(libs, Library{strings.TrimSpace(name), strings.TrimSpace(path), typ})
		}
		return libs, nil
	}
	entries, err := os.ReadDir(mediaRoot)
	if err != nil {
		return nil, nil
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(mediaRoot, name)
		if st, err := os.Stat(full); err != nil || !st.IsDir() {
			continue
		}
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") {
			continue
		}
		typ := "auto"
		if l := strings.ToLower(name); strings.Contains(l, "movie") || strings.Contains(l, "film") {
			typ = "movies"
		}
		libs = append(libs, Library{name, full, typ})
	}
	return libs, nil
}

// ParseMetadataSources parses METADATA_SOURCES="tmdb,anilist"; "" or "none" turns metadata off.
func ParseMetadataSources(spec string) ([]string, error) {
	out := []string{}
	for _, s := range strings.Split(spec, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || s == "none" {
			continue
		}
		if s != "tmdb" && s != "anilist" {
			return nil, fmt.Errorf("METADATA_SOURCES: unknown source %q (tmdb, anilist)", s)
		}
		dup := false
		for _, o := range out {
			dup = dup || o == s
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out, nil
}

// Load reads the configuration from env, or from the process environment when env is nil.
func Load(env map[string]string) (Config, error) {
	if env == nil {
		env = map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	}
	get := func(k, def string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return def
	}
	var firstErr error
	num := func(k, def string) int {
		n, err := strconv.Atoi(strings.TrimSpace(get(k, def)))
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: not a number: %q", k, get(k, def))
		}
		return n
	}
	float := func(k, def string) float64 {
		f, err := strconv.ParseFloat(strings.TrimSpace(get(k, def)), 64)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: not a number: %q", k, get(k, def))
		}
		return f
	}

	cfg := Defaults()
	cfg.APIKey = strings.TrimSpace(get("API_KEY", ""))
	if len(cfg.APIKey) < 8 {
		return cfg, fmt.Errorf("API_KEY must be set to at least 8 characters")
	}
	libs, err := LibrariesFromEnv(get("LIBRARIES", ""), get("MEDIA_ROOT", "/media"))
	if err != nil {
		return cfg, err
	}
	cfg.Libraries = libs
	cfg.CacheDir = get("CACHE_DIR", "/cache")
	cfg.ConfigDir = get("CONFIG_DIR", "/config")
	cfg.Host = get("HOST", "0.0.0.0")
	cfg.Port = num("PORT", "8097")
	cfg.ScanInterval = num("SCAN_INTERVAL", "1800")
	cfg.ProbeWorkers = max(1, num("PROBE_WORKERS", "4"))
	cfg.ThumbWorkers = max(1, num("THUMB_WORKERS", "2"))
	cfg.AudioLanguages = nil
	for _, x := range strings.Split(get("AUDIO_LANGUAGES", "jpn,ja,japanese"), ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			cfg.AudioLanguages = append(cfg.AudioLanguages, x)
		}
	}
	cfg.AudioChannels = num("AUDIO_CHANNELS", "2")
	cfg.HWAccel = strings.ToLower(get("HWACCEL", "none"))
	cfg.VAAPIDevice = get("VAAPI_DEVICE", "/dev/dri/renderD128")
	cfg.MaxHeight = num("MAX_HEIGHT", "1080")
	cfg.VideoCRF = num("VIDEO_CRF", "21")
	cfg.X264Preset = get("X264_PRESET", "veryfast")
	cfg.TranscodeThreads = max(0, num("TRANSCODE_THREADS", "4"))
	// TMDB_DEFAULT_API_KEY is baked into the image at build time (see the
	// Dockerfile), so a TMDB_API_KEY left empty in compose still falls back to it.
	cfg.TMDBAPIKey = strings.TrimSpace(get("TMDB_API_KEY", ""))
	if cfg.TMDBAPIKey == "" {
		cfg.TMDBAPIKey = strings.TrimSpace(get("TMDB_DEFAULT_API_KEY", ""))
	}
	cfg.TMDBLanguage = strings.TrimSpace(get("TMDB_LANGUAGE", "en-US"))
	if cfg.TMDBLanguage == "" {
		cfg.TMDBLanguage = "en-US"
	}
	cfg.MetadataRefreshDays = max(1, num("METADATA_REFRESH_DAYS", "30"))
	if cfg.MetadataSources, err = ParseMetadataSources(get("METADATA_SOURCES", "tmdb,anilist")); err != nil {
		return cfg, err
	}
	switch strings.ToLower(strings.TrimSpace(get("AUTO_CONVERT", "true"))) {
	case "0", "false", "no", "off":
		cfg.AutoConvert = false
	default:
		cfg.AutoConvert = true
	}
	cfg.ConvertCacheDays = max(0, float("CONVERT_CACHE_DAYS", "14"))
	cfg.ConvertCacheGB = max(0, float("CONVERT_CACHE_GB", "50"))
	cfg.FFmpeg = get("FFMPEG", "ffmpeg")
	cfg.FFprobe = get("FFPROBE", "ffprobe")
	cfg.WebDir = get("WEB_DIR", "")
	return cfg, firstErr
}
