# Improvement plan: performance, RAM, UX

Written 2026-10-16 against the Python version as of `638e768`. The pooled DB,
slimmer list queries and DB-free cached images from
[stack-evaluation.md](stack-evaluation.md) are already in. This plan
covers what is left.

Each item names the code it touches and gives a rough size: **S** is under
an hour, **M** is a few hours, **L** is a day or more. The order inside
each section is the suggested order of work.

**Summary.** The server process is no longer where the RAM goes. ffmpeg
is. A paused or forgotten transcode keeps a multi-hundred-MB process alive
indefinitely, and x264's thread count is unbounded. Those two fixes are
worth more than anything else on the Python side. On UX, the biggest
missing piece is Safari/iOS (HLS). The cheapest high-value features are
skip-intro from MKV chapters, a remembered audio track per series, and a
proper "Up next" card.

---

## 1. RAM

### 1.1 Kill ffmpeg when the player has been paused a while (S, high impact)

`server.py` `stream()` keeps ffmpeg running for as long as the socket is
open. When the video is paused, the browser stops reading and ffmpeg
blocks on a full pipe, but it stays resident. A software transcode at
1080p uses roughly 200–400 MB. A tab left paused overnight holds that
until the tab closes.

Fix this client-side in `web/app.js`. On `pause`, start a timer (about
3 min). When it fires, call `Player.stopStream()` and remember
`Player.time()`. On the next `play`, call `Player.load(savedTime)`. Direct
play needs none of this because it costs nothing while paused. The resume
path already exists: it is the same `load(t)` that seeking uses.

Don't use a server-side socket timeout for this (see 1.3): it would also
kill streams during long buffering.

### 1.2 Cap x264's threads and lookahead (S, high impact on transcodes)

`playback.ffmpeg_args()` lets libx264 pick its own thread count, which is
1.5 × cores, with frame threads that each hold their own buffers, plus a
lookahead of 20–40 frames. On an 8-core NAS that is the largest single
memory user in the system, and it competes with SMB and the rest of the
NAS for CPU.

- Add `-threads N`, from a new `TRANSCODE_THREADS` env var, default 4.
- Add `-x264-params rc-lookahead=10` (or `-rc-lookahead 10`) on the
  software path.
- Measure peak RSS of one 1080p HEVC→H.264 transcode before and after
  with `/usr/bin/time -v`. Check that the frame rate stays above 1×
  realtime on the target NAS.

### 1.3 Don't let idle keep-alive connections pin threads (S)

`Handler` inherits `timeout = None` from `StreamRequestHandler`. Each
idle browser keep-alive connection (about 6 per tab) keeps a thread
blocked in `readline()` until the browser closes it (Chrome: ~5 min).
Each thread is a stack plus a malloc arena slot.

Set `timeout = 30` on `Handler`. In `stream()` and `file()`, call
`self.connection.settimeout(None)` before sending the body, so a paused
player or a slow download is never cut off by it.

### 1.4 Hand freed memory back after a scan (S)

`Scanner.scan_library()` builds every row of a library as a dict, along
with `existing`, `dirs` and the statement list. For a 30k-file library
that is tens of MB. Python frees it, but glibc rarely returns it to the
OS. After `scan()` finishes, call `ctypes.CDLL("libc.so.6").malloc_trim(0)`
inside a `try`, so it is a no-op on musl or other platforms. It's a
one-liner, and the gain shows up in `VmRSS` right after a scan.

### 1.5 Smaller things

- **SQLite page cache.** Each of up to 4 pooled connections can grow to
  16 MB (`cache_size=-16000`). That is harmless while `library.db` is
  small, because the cache never exceeds the file. It is a 64 MB ceiling
  on big libraries, though. Use `-4000` (4 MB); the hot set is the
  `items`/`series` indexes. (S)
- **`Images._locks` grows forever.** It holds one `threading.Lock` per
  image path ever requested and never drops any. Replace it with a small
  fixed array of striped locks (`locks[hash(key) % 64]`). (S)
- **ffprobe output.** `probe.probe()` asks for `-show_streams`, which
  includes every subtitle and attachment stream. Anime MKVs often carry
  dozens of font attachments. Use `-show_entries` to request only the
  fields `summarize()` reads. That means less JSON to parse and smaller
  ffprobe processes. (S)
- **Healthcheck.** The Dockerfile's `HEALTHCHECK` starts a full Python
  interpreter every 30 s, a ~15 MB spike plus CPU on a small NAS.
  Lengthen the interval to 120 s, or use a tiny static probe binary if
  the image ever gets one. (S)

### 1.6 The big lever (already documented)

A Go rebuild takes the server from ~31–41 MB to ~9 MB idle and removes
the GIL convoy. See [stack-evaluation.md](stack-evaluation.md). That
is still the right move if the library or the number of clients grows,
but after 1.1 and 1.2 the server process is not where the RAM goes.

---

## 2. Performance

### 2.1 Conditional GETs for the JSON views (M, biggest win for page loads)

The UI already shows the cached copy first and then re-fetches
(`cached()` in `app.js`). Every navigation still costs the server a full
query, `item_json()` per row, JSON encoding and gzip. That work is where
the remaining GIL time goes.

- Keep a `generation` counter on `App` and bump it on every write: the
  scanner's `write_many`, `save_progress`, `set_watched`. A counter in
  `Database.write`/`write_many` covers all of them.
- `/api/home`, `/api/libraries/*` and `/api/series/*` send
  `ETag: "<generation>"` (plus the scan status for `/api/home`). When
  `If-None-Match` matches, answer 304 without querying.
- `send_json` currently forces `Cache-Control: no-store`. These routes
  need `no-cache` instead, so the browser revalidates.

A library page then costs about one dict lookup on revisit. Measure with
`.claude-task/tools/loadtest.py` against `/api/libraries/<name>`, sending
the header.

### 2.2 Negative cache for images that cannot be made (S)

When a thumbnail cannot be grabbed (broken file, audio-only file, ffmpeg
timeout), `Images.item_image()` returns `None` and keeps nothing. Every
scan's `pregenerate_images()` then runs ffmpeg on it again: up to 60 s
per bad file, twice (`_frame` retries without `-ss`), every 30 minutes.
Every grid view that shows it also hits SQLite and ffmpeg again after
the 5-minute 404 cache.

Write a zero-byte `item-<id>.none` marker on failure, and skip items that
have one. The scanner already removes cached images when the media file
changes, so extend that to remove the marker too.

### 2.3 Faster stream start after a seek (M)

Each seek in remux mode runs `probe.keyframe_before()`, an ffprobe pass
over up to 30 s of video read over SMB, and only then starts ffmpeg,
which opens and reads the file again. Options, cheapest first:

- Cut the read window from 30 s to the GOP size seen so far. Typical
  anime encodes use 10 s or less.
- Skip the lookup entirely and let ffmpeg seek on its own. The real
  start time is then the first fragment's `tfdt`, which the client
  would have to read to correct `Player.offset`. This is more code in
  `app.js`, but it removes one SMB round-trip per seek.

Measure time-to-first-frame after a seek on the real NAS before choosing.

### 2.4 Next-up in one query (S)

`App.home()` runs `episode_order()` for each of up to 30 active series,
then one more query per result. That is up to 60 queries, each paying
the per-row GIL cost. A single window-function query, "first unwatched
episode after the max watched (season, episode) per series", does the
same work. It is 3.5 ms today at one client, so this only matters under
the concurrency the stack evaluation measured.

### 2.5 Measured and not worth doing now

- **Rescan write amplification.** `scan_library()` upserts every row on
  every scan, changed or not. Measured on 2,400 files: a no-change rescan
  takes 0.04 s and adds ~144 KB to the WAL. Adding a `WHERE` to the
  `ON CONFLICT DO UPDATE` would save that, but it is not noticeable.
  Revisit at about 50k files.
- **Python copying the ffmpeg pipe** (`stream()`, 256 KB chunks). That is
  a few MB/s at most, which is negligible next to ffmpeg itself.

---

## 3. UX features

These fit the README's scope (no scraping, no subtitles, one user).
Ordered by value for effort.

### Quick wins (S each)

1. **A real "Up next" card.** The end-of-episode overlay shows
   `Up next: 8s`. `#p-next-title` is filled with the countdown, not the
   title. Add `next` (title, episode label, id) to the `playback_info()`
   response and show the thumbnail, `S01E06 · Title` and a countdown ring.
2. **Remember the audio track per series.** When you switch to a
   different track, the next episode goes back to auto-pick. Store the
   chosen track's language and title in `localStorage` under
   `audio:<series_id>`. On load, pass the matching index once
   `audio_tracks` is known, or have the server match by language: add an
   `alang` query parameter to `decide()`.
3. **Playback speed** (0.75–2×) in the player menu, plus `<`/`>` keys.
   `video.playbackRate` works in every mode.
4. **Picture-in-picture button** (`video.requestPictureInPicture()`),
   hidden where unsupported.
5. **Touch controls.** On phones, double-tap the left or right third to
   seek ±10 s, and show larger controls under the existing
   `@media (max-width: 700px)` block.
6. **"Mark previous episodes watched".** When you start E07 of a series
   with nothing watched, offer a one-click toast. The endpoint already
   exists (`/api/series/<id>/watched`). It needs an `up_to` id.
7. **Remove from Continue watching.** Add an × on the card that calls
   `/api/items/<id>/progress` with position 0.
8. **Keyboard help** on `?`, and `0`–`9` to jump to 0–90 %.
9. **Installable PWA.** Add a `manifest.webmanifest` and icons, so it
   opens full-screen from a phone or tablet home screen. Skip the service
   worker; there is no need to cache video.
10. **"New" markers.** Show a dot on episodes added since the series was
    last played. Both `added_at` and `played_at` are already in the
    payloads.

### Medium (M each)

11. **Skip intro / skip credits from chapters.** Many anime BD encodes
    carry chapters named `Opening`/`OP`/`Intro` and `Ending`/`ED`/`Credits`.
    - Add `-show_chapters` to `probe.probe()` and store the matched
      chapter ranges in a new column.
    - Show a "Skip intro" button while the play position is inside one.
    - Optionally, start the "Up next" countdown at the ED chapter
      instead of at end of file.

    There is no fingerprinting, no scraping and no extra CPU. A fallback
    "skip 85 s" button for files without chapters is cheap to add as
    well.
12. **Search as you type.** Debounce at 200 ms. Also match episode
    titles and filenames, not just series and movie titles.
13. **"Problems" page.** List items with `probe_error` and libraries that
    were missing at last scan, with a retry button. The data is already
    in `items.probe_error` and `scanner.status["error"]`, but the UI only
    shows the last error string.
14. **Library filters.** Add "hide watched" and "in progress" toggles, and
    an A–Z jump bar for large grids. All of it is client-side;
    `draw()` already has the fields.
15. **Night mode audio.** Apply `loudnorm` or `dynaudnorm` when audio is
    already being converted to AAC. Add a player toggle that forces the
    audio-convert path (video stays copied, so the CPU cost stays low).
16. **Backup / restore watch state.** `GET /api/state` downloads
    `state.db`, and an upload endpoint restores it. This protects the one
    volume the README says to keep.

### Large (L)

17. **HLS for Safari/iOS.** This is the main gap in the README's known
    limitations. Have ffmpeg write segmented fMP4 HLS into a per-session
    temp dir under `/cache`, and serve the playlist and segments. Use
    Safari's native HLS player for Safari/iOS only, and keep the current
    progressive path for everyone else (no hls.js needed). This needs segment cleanup and
    a per-session process lifetime. 1.1 and 1.3 are prerequisites.
18. **Seek-bar preview thumbnails.** Generate a sprite sheet with ffmpeg
    (one frame every 10 s at 160 px) in the background after probing,
    and show it on `#p-seek` hover. It costs one decode pass per file of
    CPU and about 100 KB of SSD each, so make it opt-in
    (`TRICKPLAY=1`), or generate it only for items that are actually
    played.

---

## Suggested order

| Step | Items | Why first |
|------|-------|-----------|
| 1 | 1.1, 1.2, 1.3 | Largest real RAM savings, all small and local changes |
| 2 | UX 1, 2, 7, 3 | Daily-use papercuts, client-side or one field |
| 3 | 2.1, 2.2 | Cheaper browsing; stops wasted ffmpeg runs every scan |
| 4 | UX 11 (skip intro), 13, 14 | Most-wanted features that need no new infrastructure |
| 5 | 1.4, 1.5, 2.4 | Tidy-ups, measure before/after |
| 6 | UX 17 (HLS) | Only if Safari/iOS matters; a rebuild in Go would be the moment to do it |

Measure each step with `.claude-task/tools/loadtest.py` for request
paths, and `/proc/<pid>/status` (`VmRSS`, `VmHWM`) for the server plus
`/usr/bin/time -v` for one ffmpeg run of each mode. Record the numbers
here the way stack-evaluation.md does.
