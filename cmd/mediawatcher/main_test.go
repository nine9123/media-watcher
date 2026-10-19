package main

// The real binary must exit 0 soon after SIGTERM (docker stop, idlegate)
// and SIGINT (Ctrl-C), idle and mid-scan.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

func build(t *testing.T) string {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mediawatcher-test")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "mediawatcher")
		out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type proc struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

func start(t *testing.T, dir, ffprobe string) *proc {
	media := filepath.Join(dir, "media", "Movies")
	os.MkdirAll(media, 0o755)
	os.WriteFile(filepath.Join(media, "Film (2001).mkv"), make([]byte, 2048), 0o644)
	port := freePort(t)
	cmd := exec.Command(build(t))
	cmd.Env = append(os.Environ(), "API_KEY=secret-key-123", "MEDIA_ROOT="+filepath.Join(dir, "media"),
		"CACHE_DIR="+filepath.Join(dir, "cache"), "CONFIG_DIR="+filepath.Join(dir, "config"),
		"HOST=127.0.0.1", fmt.Sprintf("PORT=%d", port), "FFPROBE="+ffprobe,
		"FFMPEG=/nonexistent/ffmpeg", "METADATA_SOURCES=")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Own process group, so leftover children (the fake ffprobe) can be cleaned up.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			// The healthcheck subcommand sees it too.
			hc := exec.Command(build(t), "healthcheck")
			hc.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
			if err := hc.Run(); err != nil {
				t.Errorf("healthcheck: %v", err)
			}
			return &proc{cmd, &out}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (p *proc) stop(t *testing.T, sig syscall.Signal, timeout time.Duration) (int, time.Duration) {
	t0 := time.Now()
	p.cmd.Process.Signal(sig)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("still running after %s:\n%s", timeout, p.out.String())
	}
	return p.cmd.ProcessState.ExitCode(), time.Since(t0)
}

func TestSignalExitsZeroWhenIdle(t *testing.T) {
	for _, c := range []struct {
		sig  syscall.Signal
		name string
	}{{syscall.SIGTERM, "SIGTERM"}, {syscall.SIGINT, "SIGINT"}} {
		t.Run(c.name, func(t *testing.T) {
			p := start(t, t.TempDir(), "/nonexistent/ffprobe")
			code, took := p.stop(t, c.sig, 6*time.Second)
			log := p.out.String()
			if code != 0 || took > 3*time.Second {
				t.Errorf("exit %d after %s:\n%s", code, took, log)
			}
			if !strings.Contains(log, "received "+c.name+", shutting down") || !strings.Contains(log, "shutdown complete") {
				t.Error(log)
			}
		})
	}
}

func TestSigtermMidScanExitsZeroAndLeavesItemForLater(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffprobe")
	os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	p := start(t, dir, fake)
	time.Sleep(time.Second) // let the scanner reach the probe step
	code, took := p.stop(t, syscall.SIGTERM, 8*time.Second)
	if code != 0 || took > 3*time.Second {
		t.Fatalf("exit %d after %s:\n%s", code, took, p.out.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cache", "library.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lib struct {
		Items []struct {
			Probed     int     `json:"probed"`
			ProbeError *string `json:"probe_error"`
		} `json:"items"`
	}
	json.Unmarshal(raw, &lib)
	// Interrupted, not failed: probed again on the next start.
	if len(lib.Items) != 1 || lib.Items[0].Probed != 0 || lib.Items[0].ProbeError != nil {
		t.Errorf("%s", raw)
	}
}
