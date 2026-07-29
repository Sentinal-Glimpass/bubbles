package main

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
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

	// Throttle state for the never-compacted-oversized-transcript stderr
	// warning (see reportOversizedTranscript). Separate from pumpMu/lastPump:
	// this is a report, not an action, and shares no correctness property with
	// the pump's escalation tiers.
	reportMu            sync.Mutex
	lastOversizedReport map[addr.Address]time.Time

	// Decoded-transcript memo, keyed by conversation path, live only for the
	// duration of one Sweep. See transcriptStats: it is what stops a sweep from
	// JSON-decoding every bubble's whole transcript two or three times over.
	statMu    sync.Mutex
	sweeping  bool
	statCache map[string]cachedStats
}

// cachedStats is one transcript's decoded Stats, exactly as transcript.Read
// returned them.
//
// There is deliberately no size/mtime stamp and no cross-sweep lifetime. A
// size+mtime key looks like the obvious invalidation rule and is not safe here:
// two writes of the same length can land on the same mtime on a
// coarse-granularity filesystem, and a cache that outlived the sweep would then
// serve a stale context size for as long as the file held still — the pump
// silently measuring a number that had already moved. Scoping the memo to one
// sweep needs no invalidation rule at all (see transcriptStats).
type cachedStats struct {
	st  transcript.Stats
	err error
}

// transcriptStats is the ONLY way this package reads a transcript. It returns
// exactly what transcript.Read would return, decoding at most once per distinct
// file state.
//
// It exists because the sweep had three full reads per cold bubble every 2
// minutes: pumpContext read it, trimTranscripts read it again for the oversized
// check, and trimTranscript read the raw bytes a third time. transcript.Read
// json.Unmarshals every line, so at the 4 MiB oversized ceiling that is ~12 MiB
// of I/O plus two full JSON decodes per parked bubble per sweep — the pump
// leaking cost in a different currency than the one it was written to save.
//
// The memo is scoped to ONE sweep (see beginSweep/endSweep), which is what
// makes it behaviour-preserving rather than merely cheaper. Within a sweep the
// only bubbles read twice are COLD ones — trimTranscripts skips hot bubbles
// outright, so the pump is the first and only reader of a live transcript —
// and a cold bubble is by definition not appending, so nothing can move under
// the memo. The single in-sweep mutation is trimTranscript's own rewrite, which
// drops a prefix and never touches the last usage-bearing entry, so the
// ContextTokens the pump reads afterwards is the same number either way.
//
// Outside a sweep every call reads afresh: no cheap file stamp is strong enough
// to trust across the 2-minute gap (see cachedStats).
func (m *HealthManager) transcriptStats(path string) (transcript.Stats, error) {
	m.statMu.Lock()
	e, ok := m.statCache[path]
	live := m.sweeping
	m.statMu.Unlock()
	if live && ok {
		return e.st, e.err
	}

	st, err := transcript.Read(path)

	m.statMu.Lock()
	if m.sweeping {
		m.statCache[path] = cachedStats{st: st, err: err}
	}
	m.statMu.Unlock()
	return st, err
}

// beginSweep/endSweep bound the memo's lifetime. Outside a sweep every read is
// a fresh read: nothing else in the process shares this manager's cadence, and
// a memo that outlives the sweep would be a cache with no invalidation event
// strong enough to trust (see cachedStats).
func (m *HealthManager) beginSweep() {
	m.statMu.Lock()
	m.sweeping = true
	m.statCache = map[string]cachedStats{}
	m.statMu.Unlock()
}

func (m *HealthManager) endSweep() {
	m.statMu.Lock()
	m.sweeping = false
	m.statCache = map[string]cachedStats{} // release the decoded Stats between sweeps
	m.statMu.Unlock()
}

// prune drops per-bubble state for bubbles that no longer exist. Without it
// lastPump and lastOversizedReport grow for the life of the process: a fleet
// that spawns and deletes workers all day would keep one entry per address ever
// seen, and the throttle state of a deleted bubble is meaningless anyway (a
// re-spawn gets a fresh address). The stats memo needs no pruning — it is
// emptied at the end of every sweep.
func (m *HealthManager) prune() {
	live := map[addr.Address]bool{}
	for _, b := range m.k.Reg.All() {
		live[b.Addr] = true
	}

	m.pumpMu.Lock()
	for a := range m.lastPump {
		if !live[a] {
			delete(m.lastPump, a)
		}
	}
	m.pumpMu.Unlock()

	m.reportMu.Lock()
	for a := range m.lastOversizedReport {
		if !live[a] {
			delete(m.lastOversizedReport, a)
		}
	}
	m.reportMu.Unlock()
}

// NewHealthManager builds the manager over a kernel.
func NewHealthManager(k *kernel.Kernel) *HealthManager {
	home, _ := os.UserHomeDir()
	return &HealthManager{
		k:                   k,
		home:                home,
		lastPump:            map[addr.Address]pumpWindows{},
		lastOversizedReport: map[addr.Address]time.Time{},
		statCache:           map[string]cachedStats{},
	}
}

// Sweep runs every health check once. Add checks here as they land.
func (m *HealthManager) Sweep() {
	m.beginSweep()
	m.trimTranscripts()
	m.pumpContext()
	m.endSweep()
	// Last: both checks above have just enumerated the registry, so anything
	// deleted since the previous sweep is dropped with this sweep's own view.
	m.prune()
}

// transcriptKeepBeforeCompact is how many lines to keep BEFORE the latest
// compaction boundary — a small safety buffer. Everything before that is dead
// history the model already summarized away.
const transcriptKeepBeforeCompact = 10

// transcriptOversizedBytes is the byte ceiling past which a COLD, NEVER
// compacted transcript is reported (see reportOversizedTranscript) instead of
// silently ignored. 4 MiB is chosen well above the size a transcript can reach
// before Task 3's pump (contextpump.go) forces /compact at ContextForceTokens
// (800k tokens, roughly a few MiB of JSONL for typical entry shapes) — so a
// file crossing this ceiling with no compaction marker anywhere in it means
// the pump has not yet had a chance to act (the bubble just went cold, or the
// force throttle window hasn't closed), not that trimming was skipped by
// mistake.
const transcriptOversizedBytes = 4 * 1024 * 1024

// oversizedReportThrottle bounds how often the stderr warning fires for the
// same bubble. Sweep runs every 2 minutes; without a throttle a bubble parked
// above the ceiling (which it stays until the pump forces a compaction that
// creates a boundary — see below) would log ~30 lines an hour for a condition
// that has not changed, the same flood contextPumpThrottle exists to prevent
// on the action side.
const oversizedReportThrottle = 30 * time.Minute

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

		// Read-only check BEFORE the trim attempt, reusing transcript.Read so
		// there is one definition of "has a compaction boundary" (the same one
		// trimTranscript's own scan and the pump in contextpump.go both answer
		// to). transcript.Read still returns valid Stats alongside
		// ErrNoUsage (a transcript with no usage-bearing entry yet, e.g. a huge
		// backlog of tool-only turns, is exactly a case worth reporting) — only
		// a hard I/O error (missing file, unreadable line) yields no usable
		// Stats and is skipped here; trimTranscript below performs its own read
		// and handles os.IsNotExist the normal way.
		st, err := m.transcriptStats(path)
		measured := err == nil || err == transcript.ErrNoUsage
		if measured && !st.HasCompaction && st.Bytes > transcriptOversizedBytes {
			m.reportOversizedTranscript(b.Addr, st.Bytes)
		}
		if measured && !st.HasCompaction {
			// A file with no compaction marker anywhere in it has no boundary
			// to cut before, so trimTranscript is a guaranteed no-op — and its
			// os.ReadFile would pull the whole (possibly multi-MiB) file in to
			// prove it. HasCompaction answers the marker question from the same
			// transcript.CompactMarker scan trimTranscript itself performs, so
			// skipping here decides nothing differently; it just declines to
			// re-read to learn what was already read.
			continue
		}

		if err := trimTranscript(path, transcriptKeepBeforeCompact); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "bubbles: transcript trim %s: %v\n", b.Addr, err)
		}
	}
}

// reportOversizedTranscript records that a's transcript has grown past
// transcriptOversizedBytes with no compaction boundary anywhere in it.
//
// DELIBERATELY NOT A TRUNCATION SITE. trimTranscript only ever cuts BEFORE the
// latest transcript.CompactMarker because everything after that marker is a
// self-contained conversation tree rooted at a parentUuid:null entry — the cut
// can never break a parentUuid chain a surviving entry depends on. A
// never-compacted file has no such boundary anywhere in it, so there is no
// byte or line offset that is safe to cut at: every surviving entry's
// parentUuid chain would run back through the discarded prefix, and severing
// it risks corrupting the conversation on --resume. Trading a token cost for a
// data-loss bug is strictly worse than the cost itself, so this function only
// ever counts and warns.
//
// The actual remedy already exists and runs elsewhere: Task 3's pump
// (contextpump.go, pumpContext) forces /compact once a HOT bubble's context
// crosses transcript.ContextForceTokens, which is what CREATES the compaction
// boundary this function is waiting for. Once that boundary exists, the next
// sweep's ordinary trimTranscript call reclaims the space safely. A bubble
// that stays cold and oversized indefinitely (no HOT window in which the pump
// could act) is exactly the case this function exists to surface, not silently
// swallow.
// The COUNTER is throttled together with the stderr line, deliberately. It
// counts REPORTS, not sweeps. Incremented per sweep it counted nothing but the
// sweep cadence: one parked bubble accrued ~30/hour for a single unchanged
// condition, and the TUI panel renders it beside genuine incident counters, so
// "OversizedTranscripts: 214" read as 214 events when it meant one file. The
// alternative — making it a gauge (Set 0/1) — was rejected because a gauge has
// to be cleared as well as set: every non-oversized bubble would need a Set(0)
// on every sweep to keep it truthful, widening the meter write surface from
// "the rare bad case" to "the whole fleet, always", and the field name is
// plural and additive like every other F* counter beside it.
//
// The parameter is `size`, not `bytes`: this file uses the bytes PACKAGE
// heavily (trimTranscript), and a parameter shadowing it here is one edit away
// from a confusing compile error in a function that must stay easy to read.
func (m *HealthManager) reportOversizedTranscript(a addr.Address, size int64) {
	m.reportMu.Lock()
	last, seen := m.lastOversizedReport[a]
	due := !seen || time.Since(last) >= oversizedReportThrottle
	if due {
		m.lastOversizedReport[a] = time.Now()
	}
	m.reportMu.Unlock()

	if due {
		m.k.Cost.Add(a, costmeter.FOversizedTranscripts, 1)
		fmt.Fprintf(os.Stderr, "bubbles: %s transcript is %d bytes with no compaction boundary — cannot safely trim (see reportOversizedTranscript); waiting for Task 3's pump to force /compact\n", a, size)
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
