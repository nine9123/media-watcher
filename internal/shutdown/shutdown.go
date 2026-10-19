// Package shutdown is the process-wide "we are stopping" signal.
//
// `docker stop` (and idlegate, which stops the container after an idle
// period) sends SIGTERM, waits 10 s, then SIGKILLs. The process exits 0
// well inside that grace period. Background work checks Stopping() to stop
// handing out items, and to avoid recording a failure that only happened
// because its ffmpeg/ffprobe child was killed along with us. Children are
// started with Context(), so they are killed when it is cancelled.
package shutdown

import (
	"context"
	"sync"
)

var (
	ctx, cancel = context.WithCancel(context.Background())
	once        sync.Once
)

// Context is cancelled when a stop signal arrives.
func Context() context.Context { return ctx }

// Stop marks the process as stopping and kills every child started with Context().
func Stop() { once.Do(cancel) }

func Stopping() bool { return ctx.Err() != nil }
