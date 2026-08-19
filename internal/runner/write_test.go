package runner

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// readAll drains r until it has n bytes or the deadline passes.
func readAll(t *testing.T, r io.Reader, n int, within time.Duration) []byte {
	t.Helper()
	type res struct{ b []byte }
	ch := make(chan res, 1)
	go func() {
		var got []byte
		buf := make([]byte, 4096)
		for len(got) < n {
			k, err := r.Read(buf)
			got = append(got, buf[:k]...)
			if err != nil {
				break
			}
		}
		ch <- res{got}
	}()
	select {
	case v := <-ch:
		return v.b
	case <-time.After(within):
		t.Fatalf("timed out waiting for %d bytes", n)
		return nil
	}
}

// newPipeSession gives a ptySession writing into a pipe rather than a PTY, so
// the test observes the exact byte stream Write emits. A real PTY would apply
// line-discipline translation (ICRNL turns the submitting CR into LF), hiding
// the very bytes these tests are about.
func newPipeSession(t *testing.T) (*ptySession, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return &ptySession{ptmx: w}, r
}

// TestWriteDeliversBodyIntactThenEnter is the contract every caller relies on:
// the body arrives byte-for-byte in order, Enter arrives last and separately,
// and the returned count is the body only.
func TestWriteDeliversBodyIntactThenEnter(t *testing.T) {
	s, tty := newPipeSession(t)
	body := bytes.Repeat([]byte("abcdefghij"), 120) // 1200 bytes: past the paste threshold
	n, err := s.Write(body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(body) {
		t.Fatalf("Write returned %d, want the body length %d (Enter must not be counted)", n, len(body))
	}
	got := readAll(t, tty, len(body)+1, 10*time.Second)
	if want := append(append([]byte{}, body...), '\r'); !bytes.Equal(got, want) {
		t.Fatalf("stream mismatch: got %d bytes, want %d ending in CR", len(got), len(want))
	}
}

// TestWriteIsPacedNotBurst is THE discriminating test. claude classifies input
// by arrival: one big burst is a paste, and a pasted slash command is never
// executed. A body past the paste threshold must therefore take real time to go
// out. Revert Write to a single ptmx.Write and this fails.
func TestWriteIsPacedNotBurst(t *testing.T) {
	s, tty := newPipeSession(t)
	body := bytes.Repeat([]byte("x"), 1200)
	go io.Copy(io.Discard, tty)

	start := time.Now()
	if _, err := s.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	elapsed := time.Since(start)

	// 1200 bytes at typeChunk per typeGap, minus the trailing Enter pause.
	minPacing := time.Duration(len(body)/typeChunk-1) * typeGap
	if elapsed < minPacing {
		t.Fatalf("Write took %v for %d bytes; a paced write needs at least %v. "+
			"It went out as one burst, which claude reads as a paste — slash commands would not run.",
			elapsed, len(body), minPacing)
	}
}

// TestWriteShortBodyIsNotSlowed: a short body is already under the paste
// threshold, so pacing must not add meaningful latency to ordinary notices.
func TestWriteShortBodyIsNotSlowed(t *testing.T) {
	s, tty := newPipeSession(t)
	go io.Copy(io.Discard, tty)
	start := time.Now()
	if _, err := s.Write([]byte("📬 New message from 0")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("a short notice took %v; pacing must stay cheap for ordinary messages", elapsed)
	}
}

// TestWritePacingIsCapped: pacing is a means, not an end. A very large body
// must not hold the session's write lock for minutes.
func TestWritePacingIsCapped(t *testing.T) {
	s, tty := newPipeSession(t)
	go io.Copy(io.Discard, tty)
	body := bytes.Repeat([]byte("y"), 512*1024) // far more than typeCap can pace
	start := time.Now()
	n, err := s.Write(body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(body) {
		t.Fatalf("wrote %d of %d bytes; the cap must not truncate the body", n, len(body))
	}
	if elapsed > typeCap+2*time.Second {
		t.Fatalf("Write took %v for %d bytes; typeCap (%v) must bound the pacing", elapsed, len(body), typeCap)
	}
}
