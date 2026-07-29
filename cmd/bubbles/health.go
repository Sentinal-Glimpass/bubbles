package main

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// HealthManager is the fleet-health daemon: background upkeep that keeps the
// fleet healthy over time, separate from the kernel's request path. Its checks
// run on a periodic Sweep. This is the seam we grow as the product matures —
// today it trims conversation transcripts; later it can reap orphans, cap disk,
// flag stale daemons, etc.
type HealthManager struct {
	k    *kernel.Kernel
	home string

	// Context-pump throttle state (see contextpump.go). It lives on the manager
	// rather than in the kernel because it is a property of THIS sweep's
	// cadence, not of the bubble: the kernel's own ceiling bounds what a bubble
	// can receive, while this bounds how often the pump asks.
	pumpMu   sync.Mutex
	lastPump map[addr.Address]pumpWindows
}

// NewHealthManager builds the manager over a kernel.
func NewHealthManager(k *kernel.Kernel) *HealthManager {
	home, _ := os.UserHomeDir()
	return &HealthManager{k: k, home: home, lastPump: map[addr.Address]pumpWindows{}}
}

// Run sweeps on a ticker until the process exits.
func (m *HealthManager) Run(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		m.Sweep()
	}
}

// Sweep runs every health check once. Add checks here as they land.
func (m *HealthManager) Sweep() {
	m.trimTranscripts()
	m.pumpContext()
}

// transcriptKeepBeforeCompact is how many lines to keep BEFORE the latest
// compaction boundary — a small safety buffer. Everything before that is dead
// history the model already summarized away.
const transcriptKeepBeforeCompact = 10

// trimTranscripts clears stale context from every COLD bubble's conversation
// transcript, keeping the file (and session id) intact so the bubble keeps
// writing to it on resume. It runs only on cold bubbles: claude appends to the
// .jsonl through a held fd, and rewriting a file it has open would lose appends
// or corrupt it. Hot bubbles are trimmed the moment they next page out.
func (m *HealthManager) trimTranscripts() {
	if m.home == "" {
		return
	}
	for _, b := range m.k.Reg.All() {
		if b.Addr.IsRoot() || b.SessionID == "" || b.Dir == "" {
			continue
		}
		if m.k.IsHot(b.Addr) {
			continue // never rewrite a transcript claude currently holds open
		}
		path := convPath(m.home, b.Dir, b.SessionID)
		if err := trimTranscript(path, transcriptKeepBeforeCompact); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "bubbles: transcript trim %s: %v\n", b.Addr, err)
		}
	}
}

// Everything after transcript.CompactMarker is a self-contained conversation
// tree rooted at a parentUuid:null entry, so cutting before it never breaks
// the active thread. See internal/transcript for the single definition of
// what a compaction boundary looks like.

// trimTranscript rewrites path in place, discarding everything before
// (latestCompaction - keepBefore) lines. No-op if there's no compaction yet or
// nothing meaningful to remove. Byte-exact: kept lines are copied verbatim (we
// never re-serialize claude's JSON). MUST only run on a file no process holds
// open (the bubble must be cold).
func trimTranscript(path string, keepBefore int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(data, []byte{'\n'}) // keeps the trailing \n on each line
	latest := -1
	for i, l := range lines {
		if bytes.Contains(l, transcript.CompactMarker) {
			latest = i
		}
	}
	if latest < 0 {
		return nil // no compaction boundary yet — nothing summarized away to clear
	}
	cut := latest - keepBefore
	if cut <= 0 {
		return nil // the whole file is at/after the buffer — nothing to remove
	}
	var buf bytes.Buffer
	buf.Grow(len(data))
	for _, l := range lines[cut:] {
		buf.Write(l)
	}
	tmp := path + ".htrim"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	// Same final path/name → same session id → the bubble keeps writing to it.
	return os.Rename(tmp, path)
}
