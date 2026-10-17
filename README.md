# Media Watcher

A small, fast, single-user media server for a NAS. It does one thing: browse
your library and play it in the browser, reliably. No actors, no user
management, no subtitles; posters and descriptions come from TMDB and
AniList.

- **Plays everything.** Each file is played the cheapest way that works:
  direct (byte ranges, zero CPU), remux (video copied, audio converted if
  needed, near-zero CPU), or transcode only when the browser cannot decode
  the video codec at all. If playback fails anyway, the player steps down
  by itself (direct → remux → full transcode) instead of buffering forever.
- **Japanese audio, always.** The audio track is auto-selected: preferred
  language first (`AUDIO_LANGUAGES`, default Japanese), commentary tracks
  last, then the default flag. You can still switch track in the player;
  the language you pick is remembered for the rest of that series.
- **Never shows subtitles.** Subtitle streams are never sent to the browser,
  in any mode.
- **Fast.** Standard-library Python + SQLite + ffmpeg, no framework. Library
  pages are one small JSON request; posters/thumbnails are pre-generated and
  downscaled on the SSD so grids never pull full-size images over SMB.
- **Watch progress.** Resume, continue watching, next up, auto-play next
  episode, mark watched/unwatched per episode or season. Library pages can
  hide watched titles or show only those in progress, and long A–Z grids
  get a letter bar.
- **Skip intro / credits** from the file's own chapters: MKVs whose
  chapters are named `Opening`/`OP`/`Intro` or `Ending`/`ED`/`Credits` get
  a skip button while that chapter plays. When the credits run to the end
  of the file the button goes straight to the next episode.
- **Light on the NAS.** A remux or transcode paused for 3 minutes stops its
  ffmpeg process; pressing play restarts it at the same spot.
- **Posters from TMDB and AniList.** Each scan looks up new series and
  movies on [TMDB](https://www.themoviedb.org/) (with a free
  `TMDB_API_KEY`) and then [AniList](https://anilist.co/) (anime, no key)
  and stores a poster and a description. See [Metadata](#metadata).
- **Download button** on every file, plus a direct URL for VLC/mpv for when
  you want to play the original.

## Quick start

```sh
echo "API_KEY=$(openssl rand -hex 24)" > .env   # see .env.example
# edit the volume paths in docker-compose.yml
docker compose up -d --build
```

Open `http://<nas>:8097` and sign in with the API key. The first scan starts
immediately; new files show up on the next scan (every 30 min by default) or
when you press ⟳ in the header.

**On TrueNAS** the compose file above can't be used as-is: a custom app has
no checkout to build from and no `.env`. The Gitea workflow in
`.gitea/workflows/build.yml` pushes an image to the registry instead; see
[docs/truenas.md](docs/truenas.md) for installing it as a custom app.

### Volumes

| Mount     | What                                              | Lose it?                                      |
|-----------|---------------------------------------------------|-----------------------------------------------|
| `/media`  | your library, mount **read-only**                 | –                                             |
| `/cache`  | SQLite library index + thumbnails. Put it on SSD. | fine, rebuilt by the next scan                |
| `/config` | `state.db` with watch progress (tiny)             | you lose watched/resume state                 |

Item ids are derived from the path inside the library, so wiping `/cache`
or changing the host mount path keeps your watch progress.

### Libraries

By default every folder directly under `/media` is a library
(`/media/Anime`, `/media/Movies`, …). A folder whose name contains "movie"
or "film" is treated as movies only; everything else is auto-detected per
file. Mount your shows *inside* a library folder, not at `/media` itself,
or every show becomes its own library.

To be explicit, set `LIBRARIES` (`Name=/path:type`, `;`-separated, type is
`auto`, `shows` or `movies`):

```
LIBRARIES=Anime=/media/anime:shows;TV=/media/tv:shows;Movies=/media/movies:movies
```

## Configuration

| Variable          | Default               | Meaning |
|-------------------|-----------------------|---------|
| `API_KEY`         | (required, ≥ 8 chars) | Login key. Also accepted as `X-Api-Key` header or `?api_key=` |
| `MEDIA_ROOT`      | `/media`              | Where libraries are auto-discovered |
| `LIBRARIES`       |                       | Explicit library list, see above |
| `AUDIO_LANGUAGES` | `jpn,ja,japanese`     | Preferred audio, in order of the language tag; titles are matched too ("Japanese 2.0") |
| `AUDIO_CHANNELS`  | `2`                   | Max channels when audio has to be converted (copied audio keeps its channels) |
| `HWACCEL`         | `none`                | `vaapi` for Intel/AMD GPU transcoding (needs `/dev/dri`, see compose file) |
| `VAAPI_DEVICE`    | `/dev/dri/renderD128` | |
| `MAX_HEIGHT`      | `1080`                | Transcodes are scaled down to this; copied video is untouched |
| `VIDEO_CRF`       | `21`                  | x264 quality for transcodes |
| `X264_PRESET`     | `veryfast`            | Use `superfast`/`ultrafast` on a weak CPU |
| `TRANSCODE_THREADS` | `4`                 | libx264 threads per transcode; `0` lets x264 choose (more RAM) |
| `SCAN_INTERVAL`   | `1800`                | Seconds between scans, `0` = only on start and on demand |
| `PROBE_WORKERS`   | `4`                   | Parallel ffprobe processes during scans |
| `THUMB_WORKERS`   | `2`                   | Parallel thumbnail generators |
| `TMDB_API_KEY`    | the image's default   | TMDB key, see below. A v3 API key or a v4 read access token |
| `TMDB_LANGUAGE`   | `en-US`               | Language of descriptions and posters |
| `METADATA_SOURCES` | `tmdb,anilist`       | Sources in the order they are tried; empty or `none` turns lookups off |
| `METADATA_REFRESH_DAYS` | `30`            | Look a title up again after this many days (new posters, late matches) |
| `PORT`            | `8097`                | |
| `LOG_LEVEL`       | `INFO`                | |

## Naming

Jellyfin/Kodi naming works as-is, and so do typical release and fansub names:

```
Anime/Frieren (2023)/Season 01/Frieren - S01E05 - Title.mkv
Anime/Frieren (2023)/Specials/Frieren - S00E01.mkv
Anime/Mushishi/Season 2/[Group] Mushishi - 03 [BD 1080p].mkv
Anime/Show.Name.S02.1080p.BluRay-GRP/Show.Name.S02E03.1080p.BluRay-GRP.mkv
Anime/Some Show/05 - Title.mkv           (bare numbers inside a series folder)
Anime/[SubsPlease] Loose Show - 12 (1080p).mkv   (joins "Loose Show" folder if one exists)
Movies/Heat (1995)/Heat (1995).mkv
```

- Series = the top-level folder inside the library, however deep the
  episodes sit below it (`Show/Show S02/Disc 1/…`, `Show/Arc 2/…` all belong
  to `Show`); the season comes from the nearest `Season NN` / `Specials` /
  `…S02…` folder. Release tags (`1080p`, `x265`, `[Group]`, …) are stripped
  from names.
- Once a show folder has recognised episodes, every other video under it
  joins that show too (unnumbered ones like `Pilot.mkv` included), and
  top-level folders with the same show name (`Show.S01.1080p-GRP`,
  `Show.S02.1080p-GRP`) merge into one series.
- `SxxEyy`, `SxxEyy-Ezz`, `1x02`, `Episode 5`, `Show - 05`/`05v2` are
  recognised. Years are never taken for episode numbers.
- Posters: `poster.jpg`/`folder.jpg`/`cover.jpg` in the series or movie
  folder, then the TMDB/AniList poster from [metadata](#metadata). Episode
  thumbnails: `<file>-thumb.jpg`. Everything else gets a frame grabbed
  from the video.
- Skipped: samples (`*-sample.mkv`, `sample.mkv`), Jellyfin extras
  (`-trailer`, `-featurette`, …), `Extras`/`Trailers`/`NC`/`Menus`/`SPs`
  folders, and NAS clutter (`@eaDir`, `.@__thumb`, `#recycle`, hidden dirs).
- Auto-cleanup: files that disappear are dropped from the library on the
  next scan, along with their thumbnails. If a library folder is missing or
  completely empty (share not mounted, NAS asleep) nothing is deleted.
- Files ffprobe cannot read and library folders that are missing are
  listed on the Problems page (`#/problems`, linked from the home page and
  the scan status), with a button to scan and retry them.

## Metadata

On by default. Every scan gets a *metadata* step after probing that looks
each series and movie up by its cleaned-up title and year, trying the
sources in `METADATA_SOURCES` order until one matches:

1. **TMDB** (series as TV shows), first result wins. Needs a key: get a free
   one at <https://www.themoviedb.org/settings/api> and set `TMDB_API_KEY`
   (either the "API Key" or the longer "API Read Access Token" works).
   Without a key TMDB is skipped.
2. **AniList**, anime only, no key. Its search is fuzzy, so a result only
   counts when one of its titles is the folder title, with or without a
   subtitle (`Frieren` finds "Frieren: Beyond Journey's End", `Heat` does
   not find "Heat Haze Days"); the folder's year breaks ties. Lookups are
   paced to stay under AniList's rate limit (about 2 s per title), so the
   first scan of a large anime library takes a while.

**Default TMDB key.** An image can carry a default key, used whenever
`TMDB_API_KEY` is not set: build with `--build-arg TMDB_API_KEY=<key>`, or
add a repository secret `TMDB_API_KEY` in Gitea and the build workflow
passes it in. The key is readable by anyone who can pull the image, so only
do this for a private registry. A `TMDB_API_KEY` set on the container
always wins.

Only titles that are new, whose folder name changed, or that were last
looked up more than `METADATA_REFRESH_DAYS` ago are sent, so a scan with
nothing new makes no requests. Changing the sources (adding a TMDB key, for
instance) looks everything up once more. If a source can't be reached or
rejects the key, the step stops and everything already fetched stays as it
was; the next scan tries again.

- A `poster.jpg`/`folder.jpg` next to the media still wins over both.
- **Wrong match?** Put the id in the folder (or movie file) name, Jellyfin
  style: `Show (2020) [tmdbid-1399]`, `Heat {tmdb-949}` or
  `Frieren [anilistid-154587]`. The tag skips the search and the other
  source, and is stripped from the displayed title.
- Series pages show the description under the title. Episodes keep their
  own names and frame thumbnails; only series and movies are looked up.
- Everything lives in `/cache` (`library.db` and `metadata/`), so wiping
  the cache only costs one more round of lookups.

This product uses the TMDB API but is not endorsed or certified by TMDB.
Anime metadata from [AniList](https://anilist.co/).

## How playback is decided

The browser reports which codecs it can decode (HEVC, HEVC 10-bit, AV1,
VP9, Opus, FLAC, AC-3, E-AC-3); H.264 8-bit and AAC/MP3 are assumed. Then:

| File                                                        | Mode      | Cost |
|-------------------------------------------------------------|-----------|------|
| MP4/WebM, playable codecs, wanted audio is the first track  | direct    | none, byte ranges |
| MKV/AVI/TS/…, or wanted audio is not the first track         | remux     | video copied; audio copied or → AAC |
| video codec not decodable (HEVC in most desktop browsers, Hi10P H.264, MPEG-2, VC-1, …) | transcode | H.264 encode, HDR tone-mapped to SDR |

The badge in the top-right of the player shows the mode; hover it for the
reason. Seeking in remux/transcode restarts ffmpeg at the new time (copied
video starts on the nearest keyframe, and the player clock is corrected to
match), so jumping around a two-hour movie is instant, not a long buffer.

## Player keys

`Space`/`K` play/pause · `←`/`→` 10 s · `J`/`L` 30 s · `↑`/`↓` volume ·
`F` fullscreen · `M` mute · `N` next episode · `<`/`>` slower/faster ·
`S` skip intro/credits · `Esc` back

## External players

`http://<nas>:8097/file/<item id>?api_key=<key>` is the original file with
range support, which VLC, mpv and Infuse can open directly. The item id is
in the player URL (`#/play/<id>`).

## Known limitations

- **Safari / iOS**: remuxed and transcoded streams are progressive
  fragmented MP4 without byte ranges, which Safari refuses. Direct-play
  files work; everything else needs HLS, which is not implemented yet.
  Chrome, Edge and Firefox are fine.
- No subtitle support at all, by design.
- VAAPI arguments are written for the Intel iHD and Mesa drivers but have
  not been tested on real hardware; if transcoding fails with
  `HWACCEL=vaapi`, switch back to `none` and check the container log.

## Development

Standard library only; ffmpeg/ffprobe must be on `PATH` (or set `FFMPEG` /
`FFPROBE`).

```sh
API_KEY=devkey123 MEDIA_ROOT=./media CACHE_DIR=./cache CONFIG_DIR=./config python -m mediawatcher
python -m pytest -q
```
