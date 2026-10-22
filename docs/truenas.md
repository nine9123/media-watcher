# Deploying on TrueNAS

Media Watcher runs on TrueNAS as a **custom app** (Apps → Discover Apps →
Custom App → **Install via YAML**). Written against the Docker-based apps in
TrueNAS 24.10 and later.

**`build: .` from `docker-compose.yml` does not work here**, and neither does
the `.env` file or the `./config` path. The Apps UI renders your YAML into a
directory of its own and runs `docker compose` *there*, not in a clone of this
repo: there is no build context, no `.env` next to it, and relative paths point
somewhere you didn't choose. A custom app can only *reference* an image, and
every setting and host path has to be written into the YAML itself. That is
what the rest of this page sets up.

## 1. Get the image into the registry

`.gitea/workflows/build.yml` builds the Dockerfile on every push to `main`
(and on demand from the Actions tab) and pushes two tags to the Gitea
registry:

```
gitea.internal:30008/nine9/media-watcher:<first 8 chars of the commit>
gitea.internal:30008/nine9/media-watcher:latest
```

The repository needs two Actions secrets (Settings → Actions → Secrets):

| Secret           | Value |
|------------------|-------|
| `REGISTRY_USER`  | the Gitea user that owns the `nine9` packages |
| `REGISTRY_TOKEN` | an access token for that user with **write:package** scope |

The runner has to be able to run `docker build`, i.e. have a Docker socket —
the same runner that builds mediavault will do. If `REGISTRY` or `IMAGE_NAME`
in the workflow differ for your setup, change them there.

The image includes the VAAPI drivers for `HWACCEL=vaapi` by default
(`intel-media-va-driver` for Intel Broadwell and newer, `mesa-va-drivers` for
AMD). Only for an Intel iGPU older than Broadwell set `EXTRA_PACKAGES` at the
top of the workflow to `i965-va-driver` and push.

The build pulls `debian:trixie-slim` (the runtime) and `golang:1.23-bookworm`
(the compiler) from Docker Hub, retrying a few times. If it still fails with
`TLS handshake timeout` on `auth.docker.io`, the runner's route to Docker Hub
is the problem, not the repository. Either fix that (DNS, MTU, proxy on the
runner host) or push copies of both images to the Gitea registry from a
machine that can reach the hub:

```
for img in debian:trixie-slim golang:1.23-bookworm; do
  docker pull "$img"
  docker tag "$img" "gitea.internal:30008/nine9/$img"
  docker push "gitea.internal:30008/nine9/$img"
done
```

and set the repository variables `BASE_IMAGE` and `GO_IMAGE` (Settings →
Actions → Variables) to `gitea.internal:30008/nine9/debian:trixie-slim` and
`gitea.internal:30008/nine9/golang:1.23-bookworm`. A pull-through mirror
works the same way.

## 2. Let TrueNAS pull from the registry

Gitea packages are private unless the owner is public, so TrueNAS has to sign
in: Apps → Installed → **Configuration** → **Sign-in to a Docker registry** →
**Add Registry**:

- **URI**: *Other Registry*, `gitea.internal:30008`
- **Username**: a Gitea user that can read the package
- **Password**: an access token with **read:package** scope (a separate one
  from the CI token, so the NAS can't push)

The NAS has to resolve `gitea.internal` itself — the runner resolving it says
nothing about the NAS. Check from the TrueNAS shell:
`docker pull gitea.internal:30008/nine9/media-watcher:latest`.

If that pull fails with `http: server gave HTTP response to HTTPS client`, the
registry is served over plain HTTP and Docker refuses it. Serve Gitea over
HTTPS (its own TLS settings or a reverse proxy) rather than editing Docker's
`daemon.json` on the NAS: TrueNAS manages that file and may rewrite it.

## 3. Create the datasets

Create these before installing; the app will not start against a host path
that doesn't exist.

| Host path (example)                         | Mounted at | Put it on | Notes |
|---------------------------------------------|------------|-----------|-------|
| `/mnt/tank/media`                           | `/media`   | anywhere  | your existing library, mounted read-only |
| `/mnt/ssd/apps/media-watcher/cache`         | `/cache`   | SSD       | library index + thumbnails; disposable |
| `/mnt/tank/apps/media-watcher/config`       | `/config`  | anywhere  | `progress.jsonl`, your watch progress — include it in snapshots/backups |

Separate datasets for cache and config means you can snapshot and replicate
`config` without dragging thousands of thumbnails along, and can destroy and
recreate `cache` without touching progress.

### Permissions

The container runs as the TrueNAS **`apps`** user (uid/gid **568**), not as
root and not as the `1000:1000` from the example compose file. So:

- `cache` and `config`: Datasets → the dataset → **Permissions** → owner user
  and group `apps`, apply recursively. Or from the shell:
  `chown -R 568:568 /mnt/ssd/apps/media-watcher/cache /mnt/tank/apps/media-watcher/config`
- `media`: `apps` needs **read and traverse** on every folder. On an
  SMB-shared dataset (NFSv4 ACL) add an ACL entry for user `apps` with
  *Read*, inherited, rather than changing the owner — the share's own users
  keep working. Don't apply recursive ownership changes to the library.

A library folder the container cannot read shows up as an empty library, not
as an error in the UI. If `/media` itself can't be listed the app exits at
start with a `PermissionError` in the log. Either way, step 5 is the check.

## 4. Install the app

Apps → Discover Apps → **Custom App** → **Install via YAML**. Name it
`media-watcher` and paste, with your paths and key:

```yaml
services:
  watcher:
    # Pin a commit tag from the registry; see "Updating" below.
    image: gitea.internal:30008/nine9/media-watcher:<sha>
    restart: unless-stopped
    user: "568:568"
    ports:
      - "8097:8097"
    environment:
      # Long random string, e.g. the output of: openssl rand -hex 24
      # Written here because a custom app has no .env file to read it from.
      API_KEY: "<your key>"
      AUDIO_LANGUAGES: "jpn,ja,japanese"
      AUDIO_CHANNELS: "2"
      HWACCEL: "none"
      SCAN_INTERVAL: "1800"
      # LIBRARIES: "Anime=/media/anime:shows;TV=/media/tv:shows;Movies=/media/movies:movies"
    volumes:
      - /mnt/tank/media:/media:ro
      - /mnt/ssd/apps/media-watcher/cache:/cache
      - /mnt/tank/apps/media-watcher/config:/config
```

Every variable in the README's configuration table can go under
`environment:`. Don't use `${API_KEY}`-style substitution: there is no `.env`
for it to read, so the value would silently be empty and the app refuses to
start with a short key.

Port 8097 must not be taken by another app or by the TrueNAS UI. If it is,
change only the left side (`"8098:8097"`).

### Hardware transcoding (Intel/AMD)

The drivers are in the image already (step 1). Find the group that owns the
render node from the TrueNAS shell:

```sh
stat -c '%g' /dev/dri/renderD128
```

and add to the service, with that number:

```yaml
    environment:
      HWACCEL: "vaapi"
    devices:
      - /dev/dri:/dev/dri
    group_add:
      - "<gid from stat>"
```

Use the number, not a group name: the name is looked up inside the container,
where it may not exist or may have a different gid than on the host. If
transcodes fail with `HWACCEL=vaapi`, set it back to `none` — direct play and
remux don't use the GPU at all.

## 5. Check it came up

```sh
curl http://<nas-ip>:8097/healthz
# {"ok": true}
```

The app's **Logs** view should show, shortly after start, one line per
library and the listening address (after the timestamp and level):

```
mediawatcher: library 'Anime' -> /media/Anime (auto)
mediawatcher: listening on http://0.0.0.0:8097
```

`no libraries found` instead means there are no folders directly under
`/media` — the host path is wrong, or it points at a single show rather than
at the folder that contains your libraries (see *Libraries* in the README).
A `PermissionError` means `apps` can't read it (step 3). Then open
`http://<nas-ip>:8097` and sign in with the API key.

## Updating

Prefer the commit tags over `latest`. With a pinned tag you know which build
is running, and rolling back is changing the tag back.

1. Push to `main`; wait for the *Build image* workflow to go green.
2. Take the new tag from the workflow log or the package page in Gitea
   (the first 8 characters of the commit).
3. Apps → Installed → media-watcher → **Edit**, change the tag, **Save**.
   TrueNAS pulls the new image and recreates the container.

`cache` and `config` survive this unchanged. If you run `:latest` instead,
saving an unchanged YAML may keep the image already on the NAS; pull it first
(Configuration → Manage Container Images → **Pull Image**, or `docker pull`
in the shell) and then restart the app.

## Behind idlegate

The image is ready to be started and stopped by idlegate:

- **Stop.** On SIGTERM (`docker stop`) the app logs `received SIGTERM,
  shutting down`, kills its running ffmpeg/ffprobe processes, and exits with
  code 0, normally in well under a second and never later than 5 s. TrueNAS
  then shows the app as Running, not Crashed. Keep idlegate's default 10 s
  `stop_grace`.
- **Health.** Use `health=/healthz` on the route. It needs no API key, and
  the port only opens once the database is ready, so the first answer is
  already a real 200.
- **Scans.** Every start runs a scan in the background, which catches up on
  the `SCAN_INTERVAL` scans missed while asleep. A scan cut off by a stop
  leaves the files it didn't finish as not probed or without a thumbnail, and
  the next start does them. It never marks them as failed.

idlegate finds the container by name, so add `container_name: media-watcher`
to the service in the YAML above and use that name in the route:

```
:8097 http://media-watcher:8097 media-watcher idle=30m health=/healthz
```

idlegate has to reach the container by that name, so both must be on the same
Docker network. Clients then connect to idlegate's port instead of the app's,
so the `ports:` mapping moves from this service to idlegate.

Only requests through idlegate keep the app awake, and a scan does not count.
A first full index of a large library can therefore take several idle periods.
To finish it in one go, run `docker stop idlegate`, which leaves the app
running, and start idlegate again once the scan is done.
