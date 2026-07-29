package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotateDaemonLogCapsInPlaceAndKeepsTheTail wires the check end to end
// against a real (temp) workspace: the daemon log must come back under its cap
// while still holding the MOST RECENT lines, because the TUI's error flash
// sends operators to this exact file to find out why a launch failed.
func TestRotateDaemonLogCapsInPlaceAndKeepsTheTail(t *testing.T) {
	base := t.TempDir()
	path := daemonLogPath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := append(bytes.Repeat([]byte("boot noise\n"), (int(daemonLogCap)/11)+1), []byte("THE ACTUAL FAILURE\n")...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	step := rotateDaemonLog(base)
	step()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > daemonLogCap {
		t.Errorf("daemon.log is %d bytes, above the %d cap", fi.Size(), daemonLogCap)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "THE ACTUAL FAILURE") {
		t.Error("rotation discarded the newest content — the operator is sent to this file to read exactly that")
	}

	// Idempotent, and safe against a workspace that has no daemon log yet.
	step()
	step()
	rotateDaemonLog(filepath.Join(base, "nonexistent"))()
}
