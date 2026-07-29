// Package logcap bounds the size of a log file on disk.
//
// The process writes two logs that used to grow without limit:
// `.bubbles/daemon.log` (opened O_APPEND with no O_TRUNC, and the sink for the
// daemon's ENTIRE stderr — throttled warnings, ngrok's output, everything) and
// `.bubbles/headroom.log` (O_TRUNC per launch, so bounded across restarts but
// unbounded within a long run). Both are the files an operator is told to read
// when something breaks, so neither may be silently emptied.
//
// There is exactly ONE capping implementation here, reached two ways:
//
//   - Rotate(path, max) caps a path whose file descriptor belongs to someone
//     else — a child process that inherited fd 2, or a handle this process
//     cannot reopen. That is daemon.log's situation.
//   - Writer is an io.Writer that appends and calls Rotate on itself once it
//     crosses the cap. That is headroom.log's situation.
//
// # Why truncate in place rather than rename
//
// The obvious rotation is rename(path, path+".1") + create a fresh path. It is
// wrong here: a writer holding an open descriptor keeps writing to the RENAMED
// inode, so every subsequent line lands in the previous generation and the log
// the operator opens stays empty forever. Rotate instead preserves the tail in
// place (the "copytruncate" strategy), which is correct no matter who holds the
// descriptor, provided they opened it O_APPEND — every writer in this repo
// does, and O_APPEND re-seeks to the (new, smaller) end on each write.
//
// # Why the tail
//
// A log truncated to its FIRST n bytes is worse than useless: it preserves the
// boot noise and discards the failure the operator opened the file to read.
// Rotate always keeps the most recent bytes.
//
// # Bound
//
// After a rotation the live log holds the newest max/2 bytes and `path.1` holds
// the bytes immediately before those, capped at max. There is exactly one
// previous generation — `.1` is overwritten, never chained to `.2` — so total
// disk for a log is bounded by 2x max. The two files are contiguous and
// non-overlapping: `.1` then the live log reads as one continuous window of
// recent history.
//
// # Known race
//
// Between reading the tail and truncating, a concurrent appender's bytes can be
// overwritten. This is inherent to copytruncate and is the price of not
// breaking a foreign descriptor; it costs at most a few lines, once per
// rotation, in a log that is by definition already overflowing.
package logcap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// PrevSuffix is appended to a log's path to name its single previous
// generation.
const PrevSuffix = ".1"

// Rotate caps path at max bytes, keeping the most recent content.
//
// It is a no-op (and not an error) when max <= 0, when path does not exist, or
// when the file is already within the cap — so it is safe to call on a fixed
// schedule and safe to call repeatedly.
//
// It returns the size of path after the call.
func Rotate(path string, max int64) (int64, error) {
	if max <= 0 {
		return 0, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	if size <= max {
		return size, nil
	}

	// keep is what stays in the live log. Half the cap, so the log has room to
	// grow again before the next rotation instead of rotating on every write.
	keep := max / 2
	if keep <= 0 {
		keep = max
	}

	f, err := os.Open(path)
	if err != nil {
		return size, err
	}
	// Read the tail BEFORE anything is moved or truncated; this is the content
	// the operator actually wants and losing it is the failure mode this whole
	// package exists to prevent.
	tail := make([]byte, keep)
	if _, err := f.ReadAt(tail, size-keep); err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return size, err
	}

	// Everything before the tail becomes the single previous generation, itself
	// capped at max so one oversized write cannot blow the 2x bound.
	oldEnd := size - keep
	oldStart := int64(0)
	if oldEnd > max {
		oldStart = oldEnd - max
	}
	if err := writePrev(path+PrevSuffix, io.NewSectionReader(f, oldStart, oldEnd-oldStart)); err != nil {
		f.Close()
		return size, err
	}
	f.Close()

	// O_APPEND holders re-seek to the end on every write, so shrinking the file
	// underneath them is safe: their next line lands right after the tail.
	w, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return size, err
	}
	defer w.Close()
	if _, err := w.WriteAt(tail, 0); err != nil {
		return size, err
	}
	if err := w.Truncate(keep); err != nil {
		return size, err
	}
	return keep, nil
}

// writePrev replaces dst with r's content via a temp file + rename, so a crash
// mid-rotation leaves the previous generation either wholly old or wholly new,
// never half-written.
func writePrev(dst string, r io.Reader) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Writer is a size-capped append-only log writer. It is safe for concurrent
// use, which matters because exec.Cmd hands one Writer to both Stdout and
// Stderr of a child process and copies each on its own goroutine.
//
// Writes are never rejected, split or dropped: a single write LARGER than the
// cap is written whole and the cap is enforced afterwards, so an oversized line
// can neither wedge the writer nor drive it into recursion.
type Writer struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens path for capped appending, creating it if needed. The existing
// content is kept (not truncated), so a log stays readable across restarts —
// Rotate is applied immediately, which is what bounds a file that grew
// unbounded before this package existed.
func Open(path string, max int64) (*Writer, error) {
	if _, err := Rotate(path, max); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{path: path, max: max, f: f, size: fi.Size()}, nil
}

// Write appends p, then caps the file if it has outgrown max.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, fmt.Errorf("logcap: write to closed %s", w.path)
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if w.max > 0 && w.size > w.max {
		// Rotate truncates in place; the descriptor stays valid because it is
		// O_APPEND. Failure here is not the caller's failure — the bytes landed.
		if sz, rerr := Rotate(w.path, w.max); rerr == nil {
			w.size = sz
		}
	}
	return n, nil
}

// Close releases the underlying file. Further writes return an error rather
// than panicking, so a late child-process copier cannot take the daemon down.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
