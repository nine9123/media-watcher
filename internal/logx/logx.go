// Package logx is a small levelled logger: "2006-01-02 15:04:05 INFO scan: message".
package logx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	LevelDebug = iota
	LevelInfo
	LevelWarning
	LevelError
)

var (
	level atomic.Int32
	std   = log.New(os.Stderr, "", 0)
)

func init() { level.Store(LevelInfo) }

// SetLevel takes a LOG_LEVEL value (DEBUG, INFO, WARNING, ERROR).
func SetLevel(name string) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG":
		level.Store(LevelDebug)
	case "WARNING", "WARN":
		level.Store(LevelWarning)
	case "ERROR", "CRITICAL":
		level.Store(LevelError)
	default:
		level.Store(LevelInfo)
	}
}

// Logger prefixes every line with its name.
type Logger struct{ name string }

func New(name string) *Logger { return &Logger{name} }

var names = [...]string{"DEBUG", "INFO", "WARNING", "ERROR"}

func (l *Logger) log(lv int32, format string, args []any) {
	if lv < level.Load() {
		return
	}
	std.Print(time.Now().Format("2006-01-02 15:04:05,000") + " " + names[lv] + " " + l.name + ": " + fmt.Sprintf(format, args...))
}

func (l *Logger) Debugf(format string, args ...any) { l.log(LevelDebug, format, args) }
func (l *Logger) Infof(format string, args ...any)  { l.log(LevelInfo, format, args) }
func (l *Logger) Warnf(format string, args ...any)  { l.log(LevelWarning, format, args) }
func (l *Logger) Errorf(format string, args ...any) { l.log(LevelError, format, args) }
