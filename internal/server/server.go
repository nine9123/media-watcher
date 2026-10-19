// Package server is the HTTP side: JSON API, web UI, direct file serving
// and ffmpeg streams.
package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/convert"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/naming"
	"github.com/nine9123/media-watcher/internal/playback"
	"github.com/nine9123/media-watcher/internal/probe"
	"github.com/nine9123/media-watcher/internal/scan"
	"github.com/nine9123/media-watcher/internal/shutdown"
	"github.com/nine9123/media-watcher/internal/store"
	"github.com/nine9123/media-watcher/web"
)

var log = logx.New("mediawatcher.http")

const (
	cookieName = "mw_session"
	chunk      = 256 * 1024
	maxBody    = 1_000_000
)

var fileTypes = map[string]string{".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".mkv": "video/x-matroska"}

// App is everything the handlers need, shared across requests.
type App struct {
	Cfg       *config.Config
	DB        *store.Store
	Images    *images.Images
	Scanner   *scan.Scanner
	Converter *convert.Converter

	token string // session cookie value: HMAC of the API key, the same as the Python version's
	// Part of every view ETag, so a restart never matches an ETag that a
	// browser kept from the previous process.
	boot string
	web  fs.FS

	streamsMu sync.Mutex
	streams   map[string]*os.Process
}

func New(cfg *config.Config, db *store.Store, im *images.Images, sc *scan.Scanner, cv *convert.Converter) *App {
	mac := hmac.New(sha256.New, []byte(cfg.APIKey))
	mac.Write([]byte("mediawatcher-session"))
	a := &App{Cfg: cfg, DB: db, Images: im, Scanner: sc, Converter: cv,
		token: hex.EncodeToString(mac.Sum(nil)), boot: strconv.FormatInt(time.Now().UnixNano(), 16),
		web: web.FS, streams: map[string]*os.Process{}}
	if cfg.WebDir != "" {
		a.web = os.DirFS(cfg.WebDir)
	}
	return a
}

// HTTPServer is the server for addr. IdleTimeout closes keep-alive
// connections a browser leaves open; video bodies have no write timeout.
func (a *App) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: a, ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second}
}

// StopStreams kills every ffmpeg stream that is still running.
func (a *App) StopStreams() {
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	for sid, p := range a.streams {
		p.Kill()
		delete(a.streams, sid)
	}
}

// ---- plumbing -------------------------------------------------------------------

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func fail(status int, msg string) *httpError {
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &httpError{status, msg}
}

type request struct {
	w      http.ResponseWriter
	r      *http.Request
	query  map[string][]string
	body   []byte
	groups []string
}

// q is a query parameter; blank counts as missing, like Python's parse_qs.
func (rq *request) q(name string) string {
	for _, v := range rq.query[name] {
		if v != "" {
			return v
		}
	}
	return ""
}

type route struct {
	method string
	re     *regexp.Regexp
	auth   bool
	fn     func(a *App, rq *request) error
}

var routes []route

func handle(method, pattern string, auth bool, fn func(a *App, rq *request) error) {
	routes = append(routes, route{method, regexp.MustCompile("^" + pattern + "$"), auth, fn})
}

func (a *App) authorized(r *http.Request) bool {
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	if key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(a.Cfg.APIKey)) == 1 {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) == 1
	}
	return false
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "mediawatcher")
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.Errorf("error handling %s %s: %v\n%s", r.Method, r.URL, v, debug.Stack())
			sendJSON(w, r, map[string]string{"error": "internal error"}, 500, nil)
		}
	}()
	rq := &request{w: w, r: r, query: r.URL.Query()}
	err := func() error {
		// Always consume the body first, so an early error response cannot
		// leave unread bytes on a keep-alive connection.
		if r.ContentLength > maxBody {
			w.Header().Set("Connection", "close")
			return fail(413, "")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			return fail(400, "cannot read body")
		}
		if len(body) > maxBody {
			w.Header().Set("Connection", "close")
			return fail(413, "")
		}
		rq.body = body
		for _, rt := range routes {
			if rt.method != r.Method && !(r.Method == http.MethodHead && rt.method == http.MethodGet) {
				continue
			}
			m := rt.re.FindStringSubmatch(r.URL.Path)
			if m == nil {
				continue
			}
			if rt.auth && !a.authorized(r) {
				return fail(401, "unauthorized")
			}
			rq.groups = m[1:]
			return rt.fn(a, rq)
		}
		return fail(404, "not found")
	}()
	if he, ok := err.(*httpError); ok {
		sendJSON(w, r, map[string]string{"error": he.msg}, he.status, nil)
	} else if err != nil {
		log.Errorf("error handling %s %s: %v", r.Method, r.URL, err)
		sendJSON(w, r, map[string]string{"error": "internal error"}, 500, nil)
	}
}

func (rq *request) bodyJSON() (map[string]any, error) {
	if len(rq.body) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(rq.body, &v); err != nil {
		return nil, fail(400, "invalid json")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fail(400, "expected an object")
	}
	return m, nil
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "application/json") || strings.HasPrefix(ctype, "text/") ||
		strings.HasPrefix(ctype, "application/javascript")
}

func sendBytes(w http.ResponseWriter, r *http.Request, body []byte, ctype string, status int, headers map[string]string) {
	h := w.Header()
	if len(body) > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && compressible(ctype) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, 5)
		zw.Write(body)
		zw.Close()
		body = buf.Bytes()
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func sendJSON(w http.ResponseWriter, r *http.Request, data any, status int, headers map[string]string) {
	body, err := json.Marshal(data)
	if err != nil {
		log.Errorf("json: %v", err)
		body, status = []byte(`{"error":"internal error"}`), 500
	}
	hs := map[string]string{"Cache-Control": "no-store"}
	for k, v := range headers {
		hs[k] = v
	}
	sendBytes(w, r, body, "application/json; charset=utf-8", status, hs)
}

func (rq *request) json(data any) error {
	sendJSON(rq.w, rq.r, data, 200, nil)
	return nil
}

func notModified(w http.ResponseWriter, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(304)
}

// view sends JSON derived only from the store (plus extra): 304 while unchanged.
func (a *App) view(rq *request, extra string, make func(d *store.Data) (any, error)) error {
	etag := fmt.Sprintf(`"%s-%d%s"`, a.boot, a.DB.Generation(), extra) // read before querying
	headers := map[string]string{"ETag": etag, "Cache-Control": "no-cache"}
	if rq.r.Header.Get("If-None-Match") == etag {
		notModified(rq.w, headers)
		return nil
	}
	var out any
	var err error
	a.DB.View(func(d *store.Data) { out, err = make(d) })
	if err != nil {
		return err
	}
	sendJSON(rq.w, rq.r, out, 200, headers)
	return nil
}

func sendStaticFile(rq *request, path, ctype, cache string) error {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return fail(404, "")
	}
	etag := fmt.Sprintf(`"%d-%d"`, st.ModTime().Unix(), st.Size())
	if rq.r.Header.Get("If-None-Match") == etag {
		notModified(rq.w, map[string]string{"ETag": etag, "Cache-Control": cache})
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fail(404, "")
	}
	sendBytes(rq.w, rq.r, body, ctype, 200, map[string]string{"ETag": etag, "Cache-Control": cache})
	return nil
}

// quote is Python's urllib.parse.quote: everything but unreserved characters and "/".
func quote(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// sendFile serves a file with byte ranges; ?download=1 saves it as name.
func sendFile(rq *request, path string, mtime float64, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return fail(404, "file not readable")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return fail(404, "file not readable")
	}
	h := rq.w.Header()
	_, ext := naming.SplitExt(filepath.Base(path))
	ctype := fileTypes[strings.ToLower(ext)]
	if ctype == "" {
		ctype = mime.TypeByExtension(strings.ToLower(ext))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "private, max-age=3600")
	if rq.q("download") != "" {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+quote(name))
	}
	sec, frac := math.Modf(mtime)
	// http.ServeContent does ranges (including suffix ranges), 416, HEAD and
	// If-Modified-Since, and sends the body with sendfile.
	http.ServeContent(rq.w, rq.r, "", time.Unix(int64(sec), int64(frac*1e9)), f)
	return nil
}

// ---- items ------------------------------------------------------------------------

func (a *App) getItem(id string) (store.Item, error) {
	it, ok := a.DB.GetItem(id)
	if !ok {
		return it, fail(404, "no such item")
	}
	return it, nil
}

func (a *App) ensureProbed(it store.Item) (store.Item, error) {
	if it.Probed == store.Probed {
		return it, nil
	}
	ok := a.Scanner.ProbeItem(it.ID, it.Path)
	it2, err := a.getItem(it.ID)
	if err != nil {
		return it2, err
	}
	if !ok {
		msg := "None"
		if it2.ProbeError != nil {
			msg = *it2.ProbeError
		}
		return it2, fail(500, "cannot read file: "+msg)
	}
	return it2, nil
}

func media(it *store.Item) *playback.Media {
	return &playback.Media{Path: it.Path, Container: it.Container, Duration: it.Duration, Streams: it.Streams}
}

func digits(s string) (*int, bool) {
	if s == "" {
		return nil, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, false
	}
	return &n, true
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// truthy is Python's bool() for a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (a *App) saveProgress(id string, position float64, duration *float64, ended bool) (bool, error) {
	it, err := a.getItem(id)
	if err != nil {
		return false, err
	}
	if duration == nil || *duration == 0 {
		duration = it.Duration
	}
	done := ended || (duration != nil && *duration != 0 && position >= *duration*watchedRatio)
	var old *store.Progress
	a.DB.View(func(d *store.Data) {
		if p, ok := d.Progress[id]; ok {
			c := *p
			old = &c
		}
	})
	p := store.Progress{ID: id, Duration: duration, UpdatedAt: now()}
	if done {
		p.Watched = true
	} else {
		p.Position = max(0, position)
		if old != nil {
			p.Watched = old.Watched // a rewatch keeps the mark until it is finished again
		}
	}
	return done, a.DB.SetProgress(p)
}

func (a *App) setWatched(ids []string, w bool) error {
	t := now()
	var recs []store.Progress
	a.DB.View(func(d *store.Data) {
		for _, id := range ids {
			p := store.Progress{ID: id, Watched: w, UpdatedAt: t}
			if old, ok := d.Progress[id]; ok {
				p.Duration = old.Duration
			}
			recs = append(recs, p)
		}
	})
	if len(recs) == 0 {
		return nil
	}
	return a.DB.SetProgress(recs...)
}

// ---- routes -----------------------------------------------------------------------

func init() {
	handle("GET", `/healthz`, false, func(a *App, rq *request) error {
		return rq.json(map[string]bool{"ok": true})
	})
	handle("GET", `/(|index\.html|app\.js|style\.css|favicon\.svg)`, false, (*App).static)
	handle("POST", `/api/login`, false, (*App).login)
	handle("POST", `/api/logout`, false, func(a *App, rq *request) error {
		sendJSON(rq.w, rq.r, map[string]bool{"ok": true}, 200,
			map[string]string{"Set-Cookie": cookieName + "=; Path=/; HttpOnly; Max-Age=0"})
		return nil
	})
	handle("GET", `/api/home`, true, (*App).apiHome)
	handle("GET", `/api/libraries/([^/]+)`, true, func(a *App, rq *request) error {
		return a.view(rq, "", func(d *store.Data) (any, error) {
			v, ok := a.library(d, rq.groups[0])
			if !ok {
				return nil, fail(404, "no such library")
			}
			return v, nil
		})
	})
	handle("GET", `/api/series/([0-9a-f]+)`, true, func(a *App, rq *request) error {
		return a.view(rq, "", func(d *store.Data) (any, error) {
			v, ok := a.series(d, rq.groups[0])
			if !ok {
				return nil, fail(404, "no such series")
			}
			return v, nil
		})
	})
	handle("GET", `/api/items/([0-9a-f]+)`, true, (*App).apiItem)
	handle("GET", `/api/search`, true, func(a *App, rq *request) error {
		var v searchView
		a.DB.View(func(d *store.Data) { v = a.search(d, rq.q("q")) })
		return rq.json(v)
	})
	handle("GET", `/api/problems`, true, func(a *App, rq *request) error {
		var v problemsView
		a.DB.View(func(d *store.Data) { v = a.problems(d) })
		st := a.Scanner.Status()
		v.LastError, v.Running = st.Error, st.Running
		return rq.json(v)
	})
	handle("GET", `/api/status`, true, func(a *App, rq *request) error { return rq.json(a.status()) })
	handle("POST", `/api/scan`, true, func(a *App, rq *request) error {
		a.Scanner.Trigger(true)
		return rq.json(map[string]bool{"ok": true})
	})
	handle("POST", `/api/items/([0-9a-f]+)/progress`, true, (*App).apiProgress)
	handle("POST", `/api/items/([0-9a-f]+)/watched`, true, func(a *App, rq *request) error {
		if _, err := a.getItem(rq.groups[0]); err != nil {
			return err
		}
		b, err := rq.bodyJSON()
		if err != nil {
			return err
		}
		w, ok := b["watched"]
		if err := a.setWatched([]string{rq.groups[0]}, !ok || truthy(w)); err != nil {
			return err
		}
		return rq.json(map[string]bool{"ok": true})
	})
	handle("POST", `/api/series/([0-9a-f]+)/watched`, true, (*App).apiSeriesWatched)
	handle("GET", `/img/(item|series)/([0-9a-f]+)`, true, (*App).image)
	handle("GET", `/api/items/([0-9a-f]+)/playback`, true, func(a *App, rq *request) error {
		info, err := a.playbackInfo(rq.groups[0], rq)
		if err != nil {
			return err
		}
		return rq.json(info)
	})
	handle("POST", `/api/streams/([A-Za-z0-9_-]+)/stop`, true, func(a *App, rq *request) error {
		a.stopStream(rq.groups[0])
		return rq.json(map[string]bool{"ok": true})
	})
	handle("GET", `/file/([0-9a-f]+)`, true, func(a *App, rq *request) error {
		// The original file with byte-range support: direct play, downloads, VLC/mpv.
		it, err := a.getItem(rq.groups[0])
		if err != nil {
			return err
		}
		return sendFile(rq, it.Path, it.Mtime, filepath.Base(it.Path))
	})
	handle("GET", `/converted/([0-9a-f]+)`, true, func(a *App, rq *request) error {
		// A finished conversion: played directly like /file, or downloaded.
		id := rq.groups[0]
		m := a.Converter.Cached(id, "")
		if m == nil {
			return fail(404, "no converted copy")
		}
		name := m.Name
		if name == "" {
			name = id + ".mp4"
		}
		created := 0.0
		if m.CreatedAt != nil { // not the mtime: that moves every time the copy is played
			created = *m.CreatedAt
		}
		return sendFile(rq, a.Converter.Path(id), created, name)
	})
	handle("GET", `/api/conversions`, true, func(a *App, rq *request) error {
		return rq.json(a.Converter.Overview())
	})
	handle("GET", `/api/conversions/([0-9a-f]+)`, true, func(a *App, rq *request) error {
		path := ""
		if it, ok := a.DB.GetItem(rq.groups[0]); ok {
			path = it.Path
		}
		return rq.json(map[string]any{"conversion": a.Converter.Status(rq.groups[0], path)})
	})
	handle("POST", `/api/items/([0-9a-f]+)/convert`, true, (*App).apiConvert)
	handle("POST", `/api/conversions/([0-9a-f]+)/cancel`, true, func(a *App, rq *request) error {
		a.Converter.Cancel(rq.groups[0])
		return rq.json(map[string]bool{"ok": true})
	})
	handle("POST", `/api/conversions/([0-9a-f]+)/delete`, true, func(a *App, rq *request) error {
		a.Converter.Delete(rq.groups[0])
		return rq.json(map[string]bool{"ok": true})
	})
	handle("GET", `/stream/([0-9a-f]+)`, true, (*App).stream)
}

func (a *App) static(rq *request) error {
	name := rq.groups[0]
	if name == "" {
		name = "index.html"
	}
	ctype := map[string]string{".html": "text/html; charset=utf-8", ".js": "application/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"}[filepath.Ext(name)]
	body, err := fs.ReadFile(a.web, name)
	if err != nil {
		return fail(404, "")
	}
	sum := sha1.Sum(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	if rq.r.Header.Get("If-None-Match") == etag {
		notModified(rq.w, map[string]string{"ETag": etag, "Cache-Control": "no-cache"})
		return nil
	}
	sendBytes(rq.w, rq.r, body, ctype, 200, map[string]string{"ETag": etag, "Cache-Control": "no-cache"})
	return nil
}

func (a *App) login(rq *request) error {
	b, err := rq.bodyJSON()
	if err != nil {
		return err
	}
	key := ""
	if v, ok := b["key"]; ok && truthy(v) {
		key = fmt.Sprint(v)
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(a.Cfg.APIKey)) != 1 {
		time.Sleep(time.Second) // slow down guessing
		return fail(401, "wrong key")
	}
	cookie := fmt.Sprintf("%s=%s; Path=/; HttpOnly; SameSite=Strict; Max-Age=%d", cookieName, a.token, 400*24*3600)
	sendJSON(rq.w, rq.r, map[string]bool{"ok": true}, 200, map[string]string{"Set-Cookie": cookie})
	return nil
}

type statusJSON struct {
	scan.Status
	Converting convert.Summary `json:"converting"`
}

func (a *App) status() statusJSON {
	return statusJSON{a.Scanner.Status(), a.Converter.Summary()}
}

func (a *App) apiHome(rq *request) error {
	status := a.status()
	raw, _ := json.Marshal(status)
	sum := sha1.Sum(raw)
	return a.view(rq, "-"+hex.EncodeToString(sum[:])[:8], func(d *store.Data) (any, error) {
		h := a.home(d)
		h.Status = status
		return h, nil
	})
}

func (a *App) apiItem(rq *request) error {
	type itemDetail struct {
		ItemJSON
		Streams   []probe.Stream `json:"streams"`
		Path      string         `json:"path"`
		Overview  *string        `json:"overview"`
		TMDBID    *int           `json:"tmdb_id"`
		AnilistID *int           `json:"anilist_id"`
	}
	var out *itemDetail
	a.DB.View(func(d *store.Data) {
		it, ok := d.Items[rq.groups[0]]
		if !ok {
			return
		}
		out = &itemDetail{ItemJSON: itemJSON(d, it), Streams: it.Streams, Path: it.RelPath}
		if out.Streams == nil {
			out.Streams = []probe.Stream{}
		}
		if m, ok := d.Meta[it.ID]; ok {
			out.Overview, out.TMDBID, out.AnilistID = m.Overview, m.TMDBID, m.AnilistID
		}
	})
	if out == nil {
		return fail(404, "no such item")
	}
	return rq.json(out)
}

func (a *App) apiProgress(rq *request) error {
	b, err := rq.bodyJSON()
	if err != nil {
		return err
	}
	pos := 0.0
	if v := b["position"]; truthy(v) {
		f, ok := toFloat(v)
		if !ok {
			return fail(400, "position must be a number")
		}
		pos = f
	}
	var dur *float64
	if v := b["duration"]; truthy(v) {
		f, ok := toFloat(v)
		if !ok {
			return fail(400, "position must be a number")
		}
		dur = &f
	}
	done, err := a.saveProgress(rq.groups[0], pos, dur, truthy(b["ended"]))
	if err != nil {
		return err
	}
	return rq.json(map[string]bool{"watched": done})
}

func (a *App) apiSeriesWatched(rq *request) error {
	b, err := rq.bodyJSON()
	if err != nil {
		return err
	}
	season, hasSeason := b["season"]
	hasSeason = hasSeason && season != nil
	var ids []string
	a.DB.View(func(d *store.Data) {
		for _, e := range episodes(d, rq.groups[0]) {
			if !hasSeason {
				ids = append(ids, e.ID)
				continue
			}
			if f, ok := season.(float64); ok && e.Season != nil && float64(*e.Season) == f {
				ids = append(ids, e.ID)
			}
		}
	})
	w, ok := b["watched"]
	if err := a.setWatched(ids, !ok || truthy(w)); err != nil {
		return err
	}
	return rq.json(map[string]bool{"ok": true})
}

func (a *App) image(rq *request) error {
	kind, id := rq.groups[0], rq.groups[1]
	// Grid pages fire dozens of these at once: serve a cached image straight
	// from disk, without touching the store. The scanner drops stale ones.
	if _, err := os.Stat(a.Images.CachePath(kind, id)); err == nil {
		return sendStaticFile(rq, a.Images.CachePath(kind, id), "image/jpeg", "private, max-age=86400")
	}
	var path string
	if kind == "item" {
		path = a.Images.ItemImage(id)
	} else {
		path = a.Images.SeriesImage(id)
	}
	if path == "" {
		rq.w.Header().Set("Content-Length", "0")
		rq.w.Header().Set("Cache-Control", "private, max-age=300")
		rq.w.WriteHeader(404)
		return nil
	}
	return sendStaticFile(rq, path, "image/jpeg", "private, max-age=86400")
}

func (a *App) apiConvert(rq *request) error {
	// Start a conversion by hand (AUTO_CONVERT off), or retry a failed one.
	it, err := a.getItem(rq.groups[0])
	if err != nil {
		return err
	}
	if it, err = a.ensureProbed(it); err != nil {
		return err
	}
	hasVideo := false
	for _, s := range it.Streams {
		hasVideo = hasVideo || s.Type == "video"
	}
	if !hasVideo {
		return fail(400, "no video stream to convert")
	}
	var title string
	a.DB.View(func(d *store.Data) { title = displayTitle(d, &it) })
	st := a.Converter.Request(it.ID, convertSource(&it), title, true)
	return rq.json(map[string]any{"conversion": st})
}

func convertSource(it *store.Item) convert.Source {
	return convert.Source{Path: it.Path, Container: it.Container, Duration: it.Duration, Streams: it.Streams}
}

// ---- playback ----------------------------------------------------------------------

type convertHint struct {
	Folder        string `json:"folder"`
	Reason        string `json:"reason"`
	Files         int    `json:"files"`
	FileCommand   string `json:"file_command"`
	FolderCommand string `json:"folder_command"`
}

type playbackJSON struct {
	Item         ItemJSON              `json:"item"`
	Mode         string                `json:"mode"`
	Reason       string                `json:"reason"`
	Video        string                `json:"video"`
	Audio        string                `json:"audio"`
	URL          string                `json:"url"`
	Start        float64               `json:"start"`
	Duration     *float64              `json:"duration"`
	MIME         *string               `json:"mime"`
	AudioTracks  []playback.AudioTrack `json:"audio_tracks"`
	AudioIndex   *int                  `json:"audio_index"`
	PrevID       *string               `json:"prev_id"`
	NextID       *string               `json:"next_id"`
	Next         *ItemJSON             `json:"next"`
	Skips        []probe.Skip          `json:"skips"`
	Convert      *convertHint          `json:"convert"`
	Conversion   *convert.Status       `json:"conversion"`
	ConvertCache map[string]float64    `json:"convert_cache"`
}

func (a *App) playbackInfo(id string, rq *request) (*playbackJSON, error) {
	it, err := a.getItem(id)
	if err != nil {
		return nil, err
	}
	if it, err = a.ensureProbed(it); err != nil {
		return nil, err
	}
	m := media(&it)
	caps := playback.ParseCaps(rq.q("vcaps"), rq.q("acaps"))
	audioIndex, _ := digits(rq.q("audio"))
	force := rq.q("force")
	if alang := strings.ToLower(strings.TrimSpace(rq.q("alang"))); audioIndex == nil && alang != "" {
		// The language last picked by hand for this series, if the file has it.
		var same []probe.Stream
		for _, s := range playback.AudioStreams(m.Streams) {
			if s.Language == alang {
				same = append(same, *s)
			}
		}
		if chosen := playback.PickAudio(same, []string{alang}); chosen != nil {
			idx := chosen.Index
			audioIndex = &idx
		}
	}
	d := playback.Decide(m, caps, a.Cfg.AudioLanguages, audioIndex, force)
	out := &playbackJSON{Duration: it.Duration, AudioTracks: playback.DescribeAudio(m.Streams), Skips: it.Skips,
		ConvertCache: map[string]float64{"days": a.Cfg.ConvertCacheDays, "gb": a.Cfg.ConvertCacheGB}}
	if out.Skips == nil {
		out.Skips = []probe.Skip{}
	}
	var title string
	var prevID, nextID *string
	var nextItem *ItemJSON
	a.DB.View(func(dd *store.Data) {
		if cur, ok := dd.Items[id]; ok {
			out.Item = itemJSON(dd, cur)
			title = displayTitle(dd, cur)
		}
		if it.SeriesID != "" {
			eps := episodes(dd, it.SeriesID)
			for i, e := range eps {
				if e.ID != id {
					continue
				}
				if i > 0 {
					p := eps[i-1].ID
					prevID = &p
				}
				if i+1 < len(eps) {
					n := eps[i+1].ID
					nextID = &n
					j := itemJSON(dd, eps[i+1])
					nextItem = &j
				}
				break
			}
		}
		if d.Mode == "transcode" && force != "transcode" && a.Cfg.HWAccel == "none" {
			out.Convert = a.convertHint(dd, &it, m, d, caps)
		}
	})

	// A file this browser can only get through a CPU transcode: use the
	// converted copy if there is one, otherwise have one made (once).
	useCopy := false
	if d.Mode == "transcode" && force != "transcode" {
		if a.Cfg.AutoConvert {
			st := a.Converter.Request(id, convertSource(&it), title, false)
			out.Conversion = &st
		} else {
			out.Conversion = a.Converter.Status(id, it.Path)
		}
		// The copy has one audio track; a different one picked by hand
		// (or remembered for the series) plays the original instead.
		if c := out.Conversion; c != nil && c.State == "done" && (audioIndex == nil || eqInt(d.AudioIndex, c.AudioIndex)) {
			a.Converter.Touch(id)
			useCopy = true
			aMode := "copy"
			if c.AudioIndex == nil {
				aMode = "none"
			}
			d = playback.Decision{Mode: "direct", VideoIndex: d.VideoIndex, AudioIndex: c.AudioIndex, Video: "copy",
				Audio: aMode, Reason: "converted copy (cached)"}
		}
	}

	var start float64
	if t := rq.q("t"); t != "" {
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, fail(400, "bad t")
		}
		start = f
	} else {
		a.DB.View(func(dd *store.Data) {
			if cur, ok := dd.Items[id]; ok {
				start = resumePoint(dd, cur)
			}
		})
	}
	if it.Duration != nil && *it.Duration != 0 {
		start = min(max(0, start), max(0, *it.Duration-1))
	}
	if d.Mode == "direct" {
		out.URL = "/file/" + id
		if useCopy {
			out.URL = "/converted/" + id
		}
	} else {
		if start > 0 && d.Video == "copy" {
			st := 0.0
			if it.StartTime != nil {
				st = *it.StartTime
			}
			if kf := probe.KeyframeBefore(shutdown.Context(), a.Cfg.FFprobe, it.Path, start, st); kf != nil && start-*kf < 30 {
				start = *kf
			}
		}
		params := []string{"t=" + fmt.Sprintf("%.3f", start), "vcaps=" + qe(rq.q("vcaps")),
			"acaps=" + qe(rq.q("acaps")), "sid=" + qe(rq.q("sid")), "force=" + qe(force)}
		if d.AudioIndex != nil {
			params = append(params, "audio="+strconv.Itoa(*d.AudioIndex))
		}
		out.URL = "/stream/" + id + "?" + strings.Join(params, "&")
		mimeType := playback.StreamMIME(a.Cfg, m, d)
		out.MIME = &mimeType
	}
	out.Mode, out.Reason, out.Video, out.Audio = d.Mode, d.Reason, d.Video, d.Audio
	out.Start, out.AudioIndex, out.PrevID, out.NextID, out.Next = start, d.AudioIndex, prevID, nextID, nextItem
	return out, nil
}

// qe is quote_plus, like Python's urlencode.
func qe(s string) string { return strings.ReplaceAll(quote(s), "%20", "+") }

func eqInt(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// convertHint gives the commands to convert this file, and the others in its
// folder that this browser would also have to transcode, so they only need a remux.
func (a *App) convertHint(d *store.Data, it *store.Item, m *playback.Media, dec playback.Decision, caps playback.Caps) *convertHint {
	folder := filepath.Dir(it.Path)
	lines := []string{playback.ConvertCommand(m, dec)}
	if it.SeriesID != "" {
		var others []*store.Item
		for _, o := range d.Items {
			if o.SeriesID == it.SeriesID && o.Probed == store.Probed && o.ID != it.ID && filepath.Dir(o.Path) == folder {
				others = append(others, o)
			}
		}
		sortByPath(others)
		for _, o := range others {
			om := media(o)
			if od := playback.Decide(om, caps, a.Cfg.AudioLanguages, nil, ""); od.Mode == "transcode" {
				lines = append(lines, playback.ConvertCommand(om, od))
			}
		}
	}
	return &convertHint{Folder: folder, Reason: dec.Reason, Files: len(lines), FileCommand: lines[0],
		FolderCommand: strings.Join(lines, "\n")}
}

func sortByPath(items []*store.Item) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
}

// ---- streams -----------------------------------------------------------------------

func (a *App) registerStream(sid string, p *os.Process) {
	if sid == "" {
		return
	}
	a.streamsMu.Lock()
	old := a.streams[sid]
	a.streams[sid] = p
	a.streamsMu.Unlock()
	if old != nil && old != p {
		old.Kill()
	}
}

func (a *App) unregisterStream(sid string, p *os.Process) {
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	if a.streams[sid] == p {
		delete(a.streams, sid)
	}
}

func (a *App) stopStream(sid string) {
	a.streamsMu.Lock()
	p := a.streams[sid]
	delete(a.streams, sid)
	a.streamsMu.Unlock()
	if p != nil {
		p.Kill()
	}
}

var startRange = regexp.MustCompile(`^bytes=0-\s*$`)

type tail struct {
	mu    sync.Mutex
	lines []string
}

func (t *tail) add(s string) {
	t.mu.Lock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 20 {
		t.lines = t.lines[1:]
	}
	t.mu.Unlock()
}

// stream remuxes/transcodes to fragmented MP4 on the fly, starting at ?t=.
func (a *App) stream(rq *request) error {
	w, r := rq.w, rq.r
	if rng := strings.TrimSpace(r.Header.Get("Range")); rng != "" && !startRange.MatchString(rng) {
		// A live stream cannot jump to a byte offset; the player seeks by time.
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(416)
		return nil
	}
	it, err := a.getItem(rq.groups[0])
	if err != nil {
		return err
	}
	if it, err = a.ensureProbed(it); err != nil {
		return err
	}
	m := media(&it)
	caps := playback.ParseCaps(rq.q("vcaps"), rq.q("acaps"))
	audioIndex, _ := digits(rq.q("audio"))
	force := rq.q("force")
	if force == "" {
		force = "remux"
	}
	d := playback.Decide(m, caps, a.Cfg.AudioLanguages, audioIndex, force)
	start := 0.0
	if t := rq.q("t"); t != "" {
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return fail(400, "bad t")
		}
		start = max(0, f)
	}
	args := playback.FFmpegArgs(a.Cfg, m, d, start)

	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	// No length and no chunking: the body ends when the connection closes,
	// which is what players expect from a live MP4.
	h.Set("Transfer-Encoding", "identity")
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return nil
	}

	log.Infof("stream %s (%s, video=%s audio=%s) from %.1fs", it.RelPath, d.Mode, d.Video, d.Audio, start)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopOnShutdown := context.AfterFunc(shutdown.Context(), cancel)
	defer stopOnShutdown()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		log.Warnf("cannot start ffmpeg for %s: %v", it.RelPath, err)
		w.WriteHeader(200)
		return nil
	}
	var errs tail
	drained := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			errs.add(sc.Text())
		}
		close(drained)
	}()
	sid := rq.q("sid")
	a.registerStream(sid, cmd.Process)
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // a paused player must not be cut off
	buf := make([]byte, chunk)
	var sent int64
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				cmd.Process.Kill() // player went away (seek, close, next episode)
				break
			}
			sent += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	<-drained
	werr := cmd.Wait()
	a.unregisterStream(sid, cmd.Process)
	if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() > 0 && len(errs.lines) > 0 {
		log.Warnf("ffmpeg exited %d for %s: %s", ee.ExitCode(), it.RelPath, strings.Join(errs.lines, "\n"))
	}
	log.Debugf("stream %s closed after %d bytes", it.ID, sent)
	return nil
}
