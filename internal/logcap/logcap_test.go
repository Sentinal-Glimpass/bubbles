package logcap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// read is a fatal-on-error file read; a missing file returns "" so a test can
// assert absence without branching.
func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestWriterKeepsMostRecentContent is the central guarantee: a log truncated to
// its FIRST n bytes preserves the boot noise and discards the failure the
// operator opened the file to read. The live log must hold the newest bytes.
func TestWriterKeepsMostRecentContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.log")
	w, err := Open(p, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("line-%03d\n", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	live := read(t, p)
	if !strings.Contains(live, "line-099") {
		t.Errorf("live log lost the most recent line; got:\n%s", live)
	}
	if strings.Contains(live, "line-000") {
		t.Errorf("live log kept the OLDEST content — rotation truncated the wrong end; got:\n%s", live)
	}
}

// TestWriterRotatesAtTheCap proves rotation actually happens and that the two
// generations are contiguous: `.1` then the live log reads as one unbroken
// window of recent history, with nothing duplicated and nothing skipped.
func TestWriterRotatesAtTheCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.log")
	const max = 64
	w, err := Open(p, max)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 40; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("%03d\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, p+PrevSuffix) == "" {
		t.Fatal("no previous generation was created — the writer never rotated")
	}
	joined := read(t, p+PrevSuffix) + read(t, p)
	if !strings.HasSuffix(joined, "039\n") {
		t.Errorf("newest record missing from the joined window: %q", joined)
	}
	// Contiguity: every record present must be consecutive with its neighbours.
	var prev = -1
	for _, ln := range strings.Split(strings.TrimSpace(joined), "\n") {
		var n int
		if _, err := fmt.Sscanf(ln, "%d", &n); err != nil {
			continue // a partial first record from a mid-line cut is expected
		}
		if prev >= 0 && n != prev+1 {
			t.Fatalf("generations are not contiguous: %d follows %d in %q", n, prev, joined)
		}
		prev = n
	}
}

// TestRotationKeepsExactlyOneGeneration pins the disk bound: no `.2`, no chain,
// total <= 2x the cap no matter how long the process runs.
func TestRotationKeepsExactlyOneGeneration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	const max = 100
	w, err := Open(p, max)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 500; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("record %04d padding padding\n", i))); err != nil {
			t.Fatal(err)
		}
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	names := map[string]bool{}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += fi.Size()
		names[e.Name()] = true
	}
	if len(names) != 2 || !names["app.log"] || !names["app.log"+PrevSuffix] {
		t.Fatalf("want exactly app.log and app.log%s, got %v", PrevSuffix, names)
	}
	if total > 2*max {
		t.Errorf("total disk %d bytes exceeds the 2x%d bound", total, max)
	}
}

// TestWriteLargerThanCapDoesNotWedge covers the degenerate input the cap logic
// could recurse on: one write bigger than the whole budget. It must be written
// whole, must not loop, and must leave the log bounded and readable.
func TestWriteLargerThanCapDoesNotWedge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	const max = 128
	w, err := Open(p, max)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	huge := append(bytes.Repeat([]byte("x"), 10*max), []byte("TAIL")...)
	n, err := w.Write(huge)
	if err != nil {
		t.Fatalf("oversized write failed: %v", err)
	}
	if n != len(huge) {
		t.Fatalf("oversized write reported %d bytes, want %d — writes must never be silently split", n, len(huge))
	}
	// The writer must still accept traffic afterwards.
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("writer wedged after an oversized write: %v", err)
	}
	live := read(t, p)
	if !strings.Contains(live, "after") {
		t.Errorf("post-oversize write missing from the live log: %q", live)
	}
	if int64(len(live)) > max {
		t.Errorf("live log is %d bytes, above the %d cap", len(live), max)
	}
	if s := read(t, p+PrevSuffix); int64(len(s)) > max {
		t.Errorf("previous generation is %d bytes, above the %d cap", len(s), max)
	}
}

// TestRotateIsIdempotentAndSafeWhenAbsent covers the periodic-sweep caller,
// which runs forever on a timer against a path that may not exist yet.
func TestRotateIsIdempotentAndSafeWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "daemon.log")
	for i := 0; i < 3; i++ {
		if n, err := Rotate(missing, 100); err != nil || n != 0 {
			t.Fatalf("Rotate on a missing path: (%d, %v), want (0, nil)", n, err)
		}
	}

	p := filepath.Join(dir, "app.log")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), 500), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Rotate(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	after := read(t, p)
	for i := 0; i < 3; i++ {
		n, err := Rotate(p, 100)
		if err != nil {
			t.Fatalf("repeat Rotate: %v", err)
		}
		if n != first {
			t.Fatalf("repeat Rotate changed the size: %d, want %d", n, first)
		}
	}
	if read(t, p) != after {
		t.Error("repeat Rotate changed a file already within the cap")
	}
	if _, err := Rotate(p, 0); err != nil {
		t.Errorf("Rotate with a non-positive cap should be a no-op: %v", err)
	}
}

// TestRotatePreservesForeignAppenders is why rotation truncates in place rather
// than renaming: a descriptor opened O_APPEND by someone else (the daemon's own
// inherited fd 2, a child process) must keep landing in the LIVE log after a
// rotation, not in the previous generation.
func TestRotatePreservesForeignAppenders(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(p, bytes.Repeat([]byte("old\n"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()

	if _, err := Rotate(p, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Write([]byte("AFTER-ROTATE\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, p), "AFTER-ROTATE") {
		t.Error("a foreign O_APPEND writer stopped landing in the live log after rotation")
	}
}

// TestWriterConcurrentWrites guards the exec.Cmd usage, where the same Writer
// is handed to both Stdout and Stderr and copied on two goroutines.
func TestWriterConcurrentWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.log")
	w, err := Open(p, 256)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := w.Write([]byte(fmt.Sprintf("g%d-%03d\n", g, i))); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestWriterCloseIsSafe: a child-process copier can outlive shutdown, so a
// write after Close must error rather than panic on a nil file.
func TestWriterCloseIsSafe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.log")
	w, err := Open(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("late")); err == nil {
		t.Error("write after Close should return an error")
	}
}
