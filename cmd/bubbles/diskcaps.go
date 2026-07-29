package main

import (
	"path/filepath"

	"github.com/Sentinal-Glimpass/bubbles/internal/logcap"
)

// Disk caps for the two logs that used to grow without limit.
//
// daemon.log is the worse of the two: it was opened O_APPEND with no O_TRUNC
// and never rotated, and it swallows the daemon's ENTIRE stderr — throttled
// warnings, ngrok's output, every launch failure. It is also the file the TUI's
// error flash tells the operator to read, so it may never be silently emptied;
// logcap keeps the most recent content plus one previous generation instead.
//
// The caps are generous on purpose. These are diagnostic logs read by a human
// after something broke; the goal is to stop unbounded growth, not to be
// frugal. 8 MiB of daemon.log is many days of a busy fleet's warnings, and the
// bound with the previous generation is 2x that.
const (
	daemonLogCap   int64 = 8 << 20 // .bubbles/daemon.log  -> <= 16 MiB with .1
	headroomLogCap int64 = 4 << 20 // .bubbles/headroom.log -> <= 8 MiB with .1
)

// daemonLogPath is the single definition of where the daemon's stderr lands.
func daemonLogPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "daemon.log")
}

// rotateDaemonLog caps daemon.log in place.
//
// It cannot use a logcap.Writer: the daemon's stderr is a descriptor the daemon
// INHERITED from the client that spawned it, and nothing in this process can
// wrap or reopen it. logcap.Rotate exists for exactly that case — it truncates
// to the most recent content in place, which an O_APPEND holder survives.
//
// Errors are swallowed deliberately: this runs on a timer forever, and a log
// that could not be trimmed this minute is not worth reporting as a check
// failure that would then show up as fleet ill-health.
func rotateDaemonLog(baseDir string) func() {
	path := daemonLogPath(baseDir)
	return func() { _, _ = logcap.Rotate(path, daemonLogCap) }
}
