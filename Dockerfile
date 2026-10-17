FROM python:3.12-slim

# Hardware transcoding drivers are optional. For Intel iGPUs (Synology,
# Unraid, most mini PCs) build with:
#   docker compose build --build-arg EXTRA_PACKAGES="intel-media-va-driver"
# AMD: EXTRA_PACKAGES="mesa-va-drivers"
ARG EXTRA_PACKAGES=""
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ${EXTRA_PACKAGES} \
 && rm -rf /var/lib/apt/lists/*

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
