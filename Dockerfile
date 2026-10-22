# Both overridable so a build host that can't reach Docker Hub can use a
# mirror or a copy in its own registry, e.g.
#   --build-arg BASE_IMAGE=gitea.internal:30008/nine9/debian:trixie-slim
#   --build-arg GO_IMAGE=gitea.internal:30008/nine9/golang:1.23-bookworm
ARG GO_IMAGE=golang:1.23-bookworm
ARG BASE_IMAGE=debian:trixie-slim

# ---- build: one static binary, the web UI embedded -------------------------
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mediawatcher ./cmd/mediawatcher

# ---- runtime: ffmpeg and the binary ----------------------------------------
FROM ${BASE_IMAGE}

# VAAPI drivers for HWACCEL=vaapi are included by default: mesa-va-drivers
# for AMD, plus intel-media-va-driver (iHD, Broadwell and newer: Synology,
# Unraid, most mini PCs) on amd64, the only architecture Debian ships it for.
# For a smaller software-only image build with
#   docker compose build --build-arg HWACCEL_DRIVERS=false
# EXTRA_PACKAGES adds further Debian packages, e.g. "i965-va-driver" for
# Intel iGPUs older than Broadwell.
ARG HWACCEL_DRIVERS=true
ARG EXTRA_PACKAGES=""
RUN set -e; \
    drivers=""; \
    if [ "${HWACCEL_DRIVERS}" = "true" ]; then \
      drivers="mesa-va-drivers"; \
      if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
        drivers="${drivers} intel-media-va-driver"; \
      fi; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates ${drivers} ${EXTRA_PACKAGES}; \
    rm -rf /var/lib/apt/lists/*

COPY --from=build /out/mediawatcher /usr/local/bin/mediawatcher

# Default TMDB key for this image, used when TMDB_API_KEY is not set at run
# time. Pass your own free key (https://www.themoviedb.org/settings/api) with
#   docker compose build --build-arg TMDB_API_KEY=...
# It ends up readable in the image, so only for images you don't publish.
ARG TMDB_API_KEY=""
ENV TMDB_DEFAULT_API_KEY=${TMDB_API_KEY}

ENV MEDIA_ROOT=/media \
    CACHE_DIR=/cache \
    CONFIG_DIR=/config \
    PORT=8097

EXPOSE 8097
# The binary checks /healthz itself (the image has no curl); a few ms per run.
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s \
  CMD ["mediawatcher", "healthcheck"]

# Exec form: the binary is PID 1 and gets SIGTERM from docker stop.
CMD ["mediawatcher"]
