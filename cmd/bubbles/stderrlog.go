package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Sentinal-Glimpass/bubbles/internal/logcap"
)

// The app's stderr is the operator's SCREEN, and that is why it must be moved.
//
// The daemon starts the app as `bubbles --hosted` under pty.Start, which points
// the child's stdin, stdout AND stderr at the same PTY. The client then relays
// that PTY to the operator's terminal. So stdout is the TUI — and stderr is the
// TUI too. Every `fmt.Fprintf(os.Stderr, "bubbles: …")` in this process paints
// raw text over whatever Bubbletea last drew, and over claude's own frame when
// the operator is dived into a bubble. Diagnostics and display shared one
// channel, so the noisier the fleet got, the more unreadable the screen became.
//
// The destination already existed and was simply never connected: .bubbles/
// daemon.log has an 8 MiB cap (diskcaps.go) and a log-rotate check that trims it
// every 5 minutes (checks.go). Its comment claims the file "swallows the
// daemon's ENTIRE stderr" — true only on the non-systemd fallback path, where
// the CLIENT opened the log before forking. Under systemd-run, the path taken on
// any machine with a user manager, nothing redirected anything and the file sat
// at zero bytes while the whole fleet's diagnostics went to the screen.
//
// Only fd 2 moves. fd 1 is the TUI and must not be touched.
//
// The redirect is done with dup2 on the descriptor rather than by reassigning
// os.Stderr, because a Go-level reassignment would not be inherited by the
// child processes that write there through it (ngrok.go hands cmd.Stderr the
// daemon's own stderr on purpose). Moving the descriptor moves everything.

// origStderr is the operator's real terminal, kept open across the redirect so
// a fatal startup error can still reach the person who typed `bubbles`. Nothing
// else may use it: a message that goes here goes on top of the TUI, which is
// exactly the problem this file exists to fix. nil once redirected, or if no
// redirect ever happened.
var origStderr *os.File

// redirectStderrToLog points fd 2 at .bubbles/daemon.log, capped and appended.
//
// It is called as early in runApp as the workspace is known, so that even the
// env-parsing warnings land in the file. Failure is deliberately not fatal:
// running with diagnostics on screen is ugly, not broken, and refusing to start
// the fleet over a log file would be a far worse trade.
func redirectStderrToLog(baseDir string) error {
	path := daemonLogPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Cap before opening, not after: the new content is appended to whatever
	// survived, and an O_APPEND holder survives logcap's in-place truncation.
	_, _ = logcap.Rotate(path, daemonLogCap)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close() // the dup below is what keeps the file open

	// Keep the terminal reachable for fatal() before overwriting fd 2.
	if dup, derr := syscall.Dup(syscall.Stderr); derr == nil {
		origStderr = os.NewFile(uintptr(dup), "/dev/stderr")
	}
	if err := dupTo(int(f.Fd()), syscall.Stderr); err != nil {
		return fmt.Errorf("dup2 fd %d -> 2: %w", f.Fd(), err)
	}
	return nil
}

// fatalf reports an error the operator MUST see. It writes to the log (where the
// full record belongs) and, when stderr has been redirected away from the
// terminal, once more to the terminal — otherwise a failure to boot would look
// like the process silently vanishing.
func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format, a...)
	if origStderr != nil {
		fmt.Fprintf(origStderr, format, a...)
	}
}
