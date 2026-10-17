# Overridable so a build host that can't reach Docker Hub can use a mirror
# or a copy in its own registry, e.g.
#   --build-arg BASE_IMAGE=gitea.internal:30008/nine9/python:3.12-slim
ARG BASE_IMAGE=python:3.12-slim
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
    apt-get install -y --no-install-recommends ffmpeg ${drivers} ${EXTRA_PACKAGES}; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY mediawatcher ./mediawatcher
COPY web ./web

# Default TMDB key for this image, used when TMDB_API_KEY is not set at run
# time. Pass your own free key (https://www.themoviedb.org/settings/api) with
#   docker compose build --build-arg TMDB_API_KEY=...
# It ends up readable in the image, so only for images you don't publish.
ARG TMDB_API_KEY=""
ENV TMDB_DEFAULT_API_KEY=${TMDB_API_KEY}

# MALLOC_ARENA_MAX: glibc otherwise keeps a malloc arena per request thread
# it has ever seen, and RSS only grows.
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    MALLOC_ARENA_MAX=2 \
    MEDIA_ROOT=/media \
    CACHE_DIR=/cache \
    CONFIG_DIR=/config \
    PORT=8097

EXPOSE 8097
# Each check starts a whole Python interpreter (~15 MB, some CPU), so not too often.
HEALTHCHECK --interval=120s --timeout=5s --start-period=10s \
  CMD python -c "import os,urllib.request; urllib.request.urlopen('http://127.0.0.1:%s/healthz' % os.environ.get('PORT', '8097'), timeout=3)"

CMD ["python", "-m", "mediawatcher"]
