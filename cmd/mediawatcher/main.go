// Command mediawatcher is a small, fast, single-user media server.
//
//	mediawatcher              run the server (configured by environment variables)
//	mediawatcher healthcheck  exit 0 if the server on PORT answers /healthz
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nine9123/media-watcher/internal/config"
	"github.com/nine9123/media-watcher/internal/convert"
	"github.com/nine9123/media-watcher/internal/images"
	"github.com/nine9123/media-watcher/internal/logx"
	"github.com/nine9123/media-watcher/internal/metadata"
	"github.com/nine9123/media-watcher/internal/scan"
	"github.com/nine9123/media-watcher/internal/server"
	"github.com/nine9123/media-watcher/internal/shutdown"
	"github.com/nine9123/media-watcher/internal/store"
)

var log = logx.New("mediawatcher")

// deadline is half of docker stop's default 10 s grace.
const deadline = 5 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	logx.SetLevel(os.Getenv("LOG_LEVEL"))
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// healthcheck is for the Dockerfile's HEALTHCHECK: the image has no curl,
// and starting the binary again costs next to nothing.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8097"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load(nil)
	if err != nil {
		return err
	}
	if len(cfg.Libraries) == 0 {
		log.Warnf("no libraries found: mount media under MEDIA_ROOT or set LIBRARIES")
	}
	for _, lib := range cfg.Libraries {
		log.Infof("library %q -> %s (%s)", lib.Name, lib.Path, lib.Type)
	}
	db, err := store.Open(cfg.CacheDir, cfg.ConfigDir)
	if err != nil {
		return err
	}
	im, err := images.New(&cfg, db)
	if err != nil {
		return err
	}
	active := metadata.Sources(&cfg)
	var md *metadata.Metadata
	if len(active) > 0 {
		md = metadata.New(&cfg, db, im, nil, nil)
	}
	if len(active) == 0 {
		log.Infof("metadata: off")
	} else {
		log.Infof("metadata: %s", strings.Join(active, ", "))
	}
	for _, s := range cfg.MetadataSources {
		if s == "tmdb" && cfg.TMDBAPIKey == "" {
			log.Infof("TMDB skipped: set TMDB_API_KEY")
		}
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	scanner := scan.New(&cfg, db, im, md)
	scanner.Start()
	conv := convert.New(&cfg)
	conv.Start()
	onOff := "off"
	if cfg.AutoConvert {
		onOff = "on"
	}
	log.Infof("auto-convert: %s (cache %s GB, %s days)", onOff,
		strconv.FormatFloat(cfg.ConvertCacheGB, 'g', -1, 64), strconv.FormatFloat(cfg.ConvertCacheDays, 'g', -1, 64))
	app := server.New(&cfg, db, im, scanner, conv)
	srv := app.HTTPServer(addr)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Infof("listening on http://%s", addr)

	select {
	case err := <-served:
		shutdown.Stop()
		db.Close()
		return err
	case sig := <-sigs:
		name := "SIGTERM"
		if sig == syscall.SIGINT {
			name = "SIGINT"
		}
		log.Infof("received %s, shutting down", name)
	}
	go func() {
		time.Sleep(deadline)
		log.Warnf("shutdown still running after %s, forcing exit", deadline)
		os.Exit(0)
	}()
	// Kills every ffmpeg/ffprobe child (streams, probes, thumbnails,
	// conversions), so nothing holds up the exit.
	shutdown.Stop()
	app.StopStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warnf("http shutdown: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Warnf("saving the library: %v", err)
	}
	log.Infof("shutdown complete")
	return nil
}
