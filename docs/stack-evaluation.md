# Stack evaluation: stay on Python, or rebuild in Go?

**Recommendation: rebuild the server in Go** (standard library +
`modernc.org/sqlite`, the same stack as `mediavault`). Keep `web/` exactly as
it is, embed it, and keep the database layout and id hashing so existing
watch progress carries over. It is the simplest stack that is clearly
better on both RAM and concurrency, and the code is small enough (≈2,100
lines of Python plus ≈500 lines of tests) to port in one focused pass.

This is not urgent. For one person on a LAN the Python version works. The
case for Go is that the problems below are built into CPython + threaded
SQLite, so they cannot be tuned away, and Go removes them without adding
complexity.

## What was measured

Test library: 100 series × 24 episodes plus one 2 GB movie (2,401 files,
indexed by the real scanner). Container limit was 3 CPUs, arm64,
Python 3.11.2, SQLite 3.40.1, Go 1.23. ffmpeg was not installed, so
streaming and thumbnailing were **not** measured. The load client is
`.claude-task/tools/loadtest.py` (N threads, each sending M sequential
requests).

| | Python (current) | Go prototype |
|---|---|---|
| RSS idle after the first scan | **~32 MB** | **~9 MB** |
| RSS after 20 concurrent clients | **230–280 MB**, never freed | ~51 MB (GC returns it over time) |
| `/api/home`, 1 client | 4 ms p50, ~250–290 req/s | – |
| `/api/home`, 8 clients | 130 ms p50, ~62 req/s | – |
| `/api/home`, 20 clients | **2.7 s p50, ~8 req/s** | – |
| 400-row query + JSON, 20 clients | ~9 queries/s (no HTTP) | **146 req/s over HTTP, 128 ms p50** |
| `/healthz`, 20 clients | ~4,100 req/s | – |
| 8 parallel 2 GB range downloads | fine: `sendfile`, ~1 s, RSS flat | `http.ServeContent`, also zero-copy |

The Go prototype was a throwaway: `net/http` plus `database/sql` running
`SELECT * FROM items ORDER BY added_at DESC LIMIT 400` on the same
`library.db` and returning it as JSON. That is a heavier response
(236 KB) than `/api/home`, so the comparison does not favour Go.

### Why Python degrades

1. **Throughput collapses on any endpoint that reads many rows.** The same
   behaviour shows up with no HTTP at all. With one SQLite connection per
   thread, `SELECT * FROM items LIMIT 400` drops from ~1,200 queries/s on
   1 thread to ~9 queries/s on 20 threads. Reusing pooled connections
   made no difference, and neither did `sys.setswitchinterval`.
   `SELECT count(*)` stays fast. So the cost is the per-row step and Row
   construction, where `sqlite3` releases and takes back the GIL for every
   row. That is the classic GIL convoy. You cannot configure it away; the
   only fixes are fewer rows per request or fewer threads.
2. **Memory creeps up and stays there.** `ThreadingHTTPServer` starts a
   thread for each connection, and `Database.conn()` opens a new SQLite
   connection per thread (with a 16 MB page cache each). glibc keeps the
   per-thread malloc arenas after those threads exit. `MALLOC_ARENA_MAX=2`
   only brought the peak down from ~280 to ~245 MB.

A real browser opens about 6 connections per host. A grid page that loads
dozens of `/img/...` requests, each doing an indexed lookup, lands at the
4–8 concurrency rows in the table: 20–130 ms instead of 3 ms. You notice it
on a slow NAS CPU, but nothing breaks.

### Keep in perspective

- **ffmpeg dominates resource use.** A software transcode takes hundreds of
  MB and most of the CPU whatever language the server is written in. A
  remux takes tens of MB. Go saves about 20–200 MB around that. It does not
  change what playback costs.
- **The Docker image is mostly ffmpeg.** Dropping `python:3.12-slim` saves
  ~50 MB of a ~400 MB image. Nice to have, but not the reason to switch.

## Options considered

| Stack | RAM | Concurrency | Complexity | Verdict |
|---|---|---|---|---|
| **Go** (stdlib `net/http`, `os/exec`, `embed`, `modernc.org/sqlite`) | ~10 MB idle | goroutines, real parallelism, pooled DB | low: one static binary, one dependency | **pick this** |
| Python, patched | ~30 MB idle, spikes | still GIL-bound | lowest, no rewrite | fallback if a rewrite is not wanted (see below) |
| Rust (axum + tokio + rusqlite) | ~5 MB idle | excellent | high: async + borrow checker for a ~2k-line app, slow builds on a NAS | overkill |
| Node / Bun | 50–80 MB idle | event loop is fine for this | medium: npm dependency churn, native sqlite module | no gain over Go |
| C# / Java | 60–150 MB | good | heavy runtime | wrong fit |

Why Go fits this app in particular:

- **Almost everything is in the standard library.** `http.ServeContent`
  does byte ranges, `If-Modified-Since`, HEAD and 416, which replaces all
  of `file()`. `os/exec` + `io.Copy` handles the ffmpeg stdout → response
  pipe, and `exec.CommandContext` with the request context kills ffmpeg
  when the browser disconnects. `embed.FS` serves `web/`.
  `encoding/json`, `regexp`, `crypto/hmac` and `crypto/sha1` cover the
  rest. The only third-party package is `modernc.org/sqlite`, which needs
  no cgo, so the binary stays static.
- **The patterns already exist in `mediavault`:** the walk → workers →
  single-writer scanner, WAL setup, a multi-stage Dockerfile with ffmpeg in
  the runtime image, and env-based config. Sharing them lowers the
  maintenance cost of two codebases.
- **Simple concurrency.** One goroutine per request, a `database/sql` pool
  capped at `NumCPU`, and a bounded worker pool for ffprobe/thumbnails
  replace `ThreadPoolExecutor`, `threading.local` and the stderr drain
  thread.

Downsides:

- `modernc.org/sqlite` is a C-to-Go translation. A single query ran about
  2× slower than CPython's native libsqlite (245 vs 517 queries/s on one
  thread). It only wins under concurrency. Watch for this if the `/api/home`
  queries ever get heavy, and add indexes before reaching for cgo.
- **The filename parser is the one fiddly part of the port.** Go's
  `regexp` is RE2, which has no lookahead or lookbehind, and `naming.py`
  uses them in 8 patterns (`(?<![a-z0-9])s(\d{1,2})…(?![0-9])`,
  `(?=$|[\s._\-\[(])`, …). Each one needs rewriting: either consume the
  boundary character in a group and check the neighbouring bytes in code,
  or match on padded input. The 162 lines of naming tests port directly to
  a table-driven `naming_test.go` and are the safety net.

## If you rebuild: suggested shape

```
cmd/mediawatcher/main.go    config, open DB, start scanner, serve
internal/config             env vars, same names and defaults as config.py
internal/store              schema (unchanged), sha1 item/series ids (unchanged)
internal/naming             port of naming.py + table tests from test_naming.py
internal/probe              ffprobe JSON → streams
internal/playback           decide() + ffmpeg_args(), tests from test_playback.py
internal/scan               walk → probe workers → single writer
internal/images             ffmpeg poster/thumb generation
internal/server             routes from server.py; web/ via embed.FS
```

Compatibility to keep, so the switch is a drop-in container swap:

- `item_id = sha1(library + "\0" + rel_path)[:16]` and
  `series_id = sha1("series\0" + library + "\0" + key)[:16]`. Matching ids
  let `state.db` (watch progress) carry over untouched, and the
  `library.db` cache can simply be rebuilt.
- The same env variables, ports, volumes and HTTP API, so `web/app.js`
  needs no changes.

Order: naming + playback first (pure functions, tests already written),
then store + scanner, then the server. Run both versions against the same
library and compare `/api/libraries/*` JSON before switching over.

## If you stay on Python: cheapest mitigations

These do not fix the GIL convoy. They make it hurt less:

- Read only the columns each page needs instead of `i.*` (the `streams`
  JSON blob is the large one), and add an index on `items(added_at)` for
  the "recently added" query.
- Use a small fixed set of DB connections instead of one per short-lived
  request thread, and set `MALLOC_ARENA_MAX=2` in the Dockerfile to cap
  arena growth.
- Serve thumbnails as static files by path without a DB lookup, so grid
  pages never touch SQLite per image.
