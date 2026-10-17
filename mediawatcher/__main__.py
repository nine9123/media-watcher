"""Entry point: python -m mediawatcher"""

from __future__ import annotations

import logging
import os

from . import config
from .db import Database
from .images import Images
from .metadata import Metadata, sources
from .scanner import Scanner
from .server import App, make_server


def main() -> None:
    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    cfg = config.load()
    log = logging.getLogger("mediawatcher")
    if not cfg.libraries:
        log.warning("no libraries found: mount media under MEDIA_ROOT or set LIBRARIES")
    for lib in cfg.libraries:
        log.info("library %r -> %s (%s)", lib.name, lib.path, lib.type)
    db = Database(cfg.cache_dir, cfg.config_dir)
    images = Images(cfg, db)
    active = sources(cfg)
    metadata = Metadata(cfg, db, images) if active else None
    log.info("metadata: %s", ", ".join(active) or "off")
    if "tmdb" in cfg.metadata_sources and not cfg.tmdb_api_key:
        log.info("TMDB skipped: set TMDB_API_KEY")
    scanner = Scanner(cfg, db, images, metadata)
    scanner.start()
    server = make_server(App(cfg, db, images, scanner))
    log.info("listening on http://%s:%d", cfg.host, cfg.port)
    try:
        server.serve_forever(poll_interval=1.0)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
