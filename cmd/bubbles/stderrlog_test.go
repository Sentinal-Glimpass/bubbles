package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withFakeTerminal replaces fd 2 with a pipe standing in for the operator's
// terminal, and returns a function reading whatever reached it. It restores the
// real fd 2 on cleanup so one test cannot blind the rest of the run.
func withFakeTerminal(t *testing.T) func() string {
	t.Helper()
	saved, err := syscall.Dup(syscall.Stderr)
	if err != nil {
		t.Fatalf("dup stderr: %v", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := dupTo(int(w.Fd()), syscall.Stderr); err != nil {
		t.Fatalf("dup2: %v", err)
	}
	savedOrig := origStderr
	t.Cleanup(func() {
		_ = dupTo(saved, syscall.Stderr)
		_ = syscall.Close(saved)
		origStderr = savedOrig
		r.Close()
		w.Close()
	})
	return func() string {
		// Every write end must be closed or the read below blocks forever:
		// w itself, and origStderr, which is a dup of fd 2 taken while fd 2
		// still pointed at the pipe.
		w.Close()
		if origStderr != nil {
			origStderr.Close()
			origStderr = nil
		}
		buf := make([]byte, 64*1024)
		n, _ := r.Read(buf)
		return string(buf[:n])
	}
}

// TestStderrGoesToTheLogNotTheScreen is the discriminating test for the whole
// change: under the daemon this process's stderr is the terminal the TUI draws
// on, so a diagnostic must reach the log file and must NOT reach the terminal.
// Delete the redirect and the "screen" assertion fails.
func TestStderrGoesToTheLogNotTheScreen(t *testing.T) {
	base := t.TempDir()
	screen := withFakeTerminal(t)

	if err := redirectStderrToLog(base); err != nil {
		t.Fatalf("redirectStderrToLog: %v", err)
	}
	const line = "bubbles: transcript trim outcome=refused-recent addr=0.6\n"
	fmt.Fprint(os.Stderr, line)

	got, err := os.ReadFile(daemonLogPath(base))
	if err != nil {
		t.Fatalf("read daemon.log: %v", err)
	}
	if !strings.Contains(string(got), line) {
		t.Fatalf("daemon.log does not contain the diagnostic; got %q", string(got))
	}
	if onScreen := screen(); strings.Contains(onScreen, "transcript trim") {
		t.Fatalf("the diagnostic reached the operator's terminal: %q", onScreen)
	}
}

// TestChildProcessesInheritTheLog: ngrok and any other child handed this
// process's stderr must land in the file too. This is why the redirect moves the
// descriptor instead of reassigning os.Stderr — a Go-level reassignment would
// not be inherited.
func TestChildProcessesInheritTheLog(t *testing.T) {
	base := t.TempDir()
	_ = withFakeTerminal(t)
	if err := redirectStderrToLog(base); err != nil {
		t.Fatalf("redirectStderrToLog: %v", err)
	}
	if syscall.Stderr != 2 {
		t.Fatalf("stderr fd is %d, want 2", syscall.Stderr)
	}
	// A child inheriting fd 2 writes to the same open file description. This
	// writes to the descriptor directly rather than wrapping it in os.NewFile,
	// which would attach a finalizer that closes fd 2 out from under the rest
	// of the run once the wrapper is collected.
	if _, err := syscall.Write(syscall.Stderr, []byte("bubbles: child line\n")); err != nil {
		t.Fatalf("write to inherited fd 2: %v", err)
	}
	got, _ := os.ReadFile(daemonLogPath(base))
	if !strings.Contains(string(got), "child line") {
		t.Fatalf("an inherited fd 2 did not reach daemon.log; got %q", string(got))
	}
}

// TestFatalStillReachesTheOperator: hiding diagnostics must not hide a failed
// boot. fatalf writes to the log AND back to the real terminal.
func TestFatalStillReachesTheOperator(t *testing.T) {
	base := t.TempDir()
	screen := withFakeTerminal(t)
	if err := redirectStderrToLog(base); err != nil {
		t.Fatalf("redirectStderrToLog: %v", err)
	}
	fatalf("bubbles: %v\n", "daemon did not start")

	if onScreen := screen(); !strings.Contains(onScreen, "daemon did not start") {
		t.Fatalf("a fatal error never reached the operator's terminal; got %q", onScreen)
	}
	got, _ := os.ReadFile(daemonLogPath(base))
	if !strings.Contains(string(got), "daemon did not start") {
		t.Fatalf("a fatal error was not recorded in daemon.log; got %q", string(got))
	}
}

// TestRedirectCapsTheLogOnStart: the file is bounded at startup, not only by the
// 5-minute rotate check, so a daemon that crash-loops cannot grow it unbounded
// between rotations.
func TestRedirectCapsTheLogOnStart(t *testing.T) {
	base := t.TempDir()
	_ = withFakeTerminal(t)
	path := daemonLogPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	oversize := make([]byte, daemonLogCap+(1<<20))
	for i := range oversize {
		oversize[i] = 'x'
	}
	oversize[len(oversize)-1] = '\n'
	if err := os.WriteFile(path, oversize, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := redirectStderrToLog(base); err != nil {
		t.Fatalf("redirectStderrToLog: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > daemonLogCap {
		t.Fatalf("daemon.log is %d bytes after start, above the %d cap", st.Size(), daemonLogCap)
	}
}
