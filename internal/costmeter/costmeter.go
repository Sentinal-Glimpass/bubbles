// Package costmeter records per-bubble cost and efficiency counters (notices
// written/suppressed/capped, deliveries, triggered turns, context evictions and
// rewarms, context token usage). Without this telemetry the later phases of the
// cost/efficiency overhaul are unfalsifiable -- a win and a regression would
// look identical from the outside.
package costmeter

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Field selects which counter in a Counters an Add or Set call touches, so
// callers don't need a separate method per counter.
type Field int

// The counters tracked per bubble. Written/Suppressed/Capped describe
// notice production; Inline/ViaTool describe how a delivered notice reached the
// model; TurnsTriggered counts turns a notice caused to run; Evictions/Rewarms
// describe context-window churn; ContextTokens is a live gauge, not a running
// total (see Set); OversizedTranscripts counts REPORTS of a COLD bubble's
// never-compacted transcript over the byte ceiling (see cmd/bubbles/health.go)
// -- observability only, since that transcript is deliberately never
// truncated. It is throttled with the warning it accompanies, so it counts
// distinct reports rather than sweeps: a parked bubble must not read as an
// incident count that climbs with the sweep cadence.
// RelaunchesSuppressed counts relaunches the crash-loop backoff refused (a
// window still open, or the bubble given up on): every suppression path in this
// repo increments a counter, so that a decision NOT to act is as visible in the
// telemetry as an action. It deliberately counts refusals, not distinct
// bubbles -- the cost being avoided is per attempt.
// CompactsExpired counts DEFERRED compactions the kernel gave up on: a
// compact() call whose session never went output-silent (or whose operator
// never stopped typing) before the pending entry hit its bound. A compaction
// that silently never happens is the exact failure this counter exists to make
// visible -- it was invisible before, and cost real money. Its three siblings
// cover the rest of that family, because a deferred compaction can fail in more
// than one way and each way needs its own diagnosis: CompactsDropped counts
// pending compactions discarded because the bubble went cold or died before the
// command could be typed (discarding is correct -- waking it would pay a full
// prompt-cache rewarm -- but a silent discard is indistinguishable from a lost
// one); CompactsRetried counts commands that were written but provoked no
// output at all, i.e. were swallowed by a session that was not accepting input,
// CompactsAccepted counts the ONLY positive outcome: a written /compact the
// session visibly reacted to. Without it every compact counter measures a
// failure, so an all-zero fleet reads identically whether every compaction
// landed, no bubble ever called compact(), or the flush check never ran — which
// is exactly the silence the original swallowed-keystroke bug lived in for 7+
// calls and 792k of billed context. "Accepted" not "succeeded": it evidences
// receipt of the keystrokes, not a completed summarization.
// and so were re-issued; CompactsAbandoned counts the give-up after
// maxCompactWrites of that.
// TranscriptsTrimmed / TranscriptBytesArchived / TrimsRefused cover the one
// code path in this repo that rewrites a user's conversation history (see
// cmd/bubbles/health.go). Trimming used to be entirely silent, which is why a
// lost day of work could only be reconstructed by forensic archaeology:
// TranscriptsTrimmed counts rewrites, TranscriptBytesArchived counts the bytes
// moved out of the live transcript and into its append-only .archive sidecar
// (bytes, not files, because "how much history moved" is the question asked
// after the fact), and TrimsRefused counts every attempt that declined to
// rewrite — a stale identity, a file still being written to, a hot bubble, or
// an I/O failure. A refusal is the system working, but a refusal nobody can see
// is how this incident stayed invisible for two weeks.
//
// The F* constants are an iota block and Counters is persisted, so new fields
// are APPENDED and existing ones are NEVER renumbered — a renumber silently
// re-labels every counter recorded before it (see fields_test.go).
const (
	FNoticesWritten Field = iota
	FNoticesSuppressed
	FNoticesCapped
	FDeliveriesInline
	FDeliveriesViaTool
	FTurnsTriggered
	FEvictions
	FRewarms
	FContextTokens
	FOversizedTranscripts
	FRelaunchesSuppressed
	FCompactsExpired
	FCompactsDropped
	FCompactsRetried
	FCompactsAbandoned
	FCompactsAccepted
	FTranscriptsTrimmed
	FTranscriptBytesArchived
	FTrimsRefused
	// FPumpsDeferred counts background context-pump actions withheld because the
	// operator was dived into that bubble. It is a suppression, so it is metered
	// like every other one — an unrecorded skip is indistinguishable from a pump
	// that never had anything to do, which is exactly how the compact() bug hid.
	FPumpsDeferred
)

// Counters holds one bubble's cost/efficiency tally. All fields are int64 so
// they persist and compare cleanly regardless of platform word size.
type Counters struct {
	NoticesWritten       int64
	NoticesSuppressed    int64
	NoticesCapped        int64
	DeliveriesInline     int64
	DeliveriesViaTool    int64
	TurnsTriggered       int64
	Evictions            int64
	Rewarms              int64
	ContextTokens        int64
	OversizedTranscripts int64
	RelaunchesSuppressed int64
	CompactsExpired      int64
	CompactsDropped      int64
	CompactsRetried      int64
	CompactsAbandoned    int64
	CompactsAccepted     int64

	PumpsDeferred        int64
	TranscriptsTrimmed      int64
	TranscriptBytesArchived int64
	TrimsRefused            int64
}

// field maps f to the corresponding pointer inside c, so Add and Set can share
// one switch instead of each duplicating it. An unknown Field returns nil; both
// callers treat that as a safe no-op rather than a panic, since a counter
// recording bug should never be able to crash a bubble.
func field(c *Counters, f Field) *int64 {
	switch f {
	case FNoticesWritten:
		return &c.NoticesWritten
	case FNoticesSuppressed:
		return &c.NoticesSuppressed
	case FNoticesCapped:
		return &c.NoticesCapped
	case FDeliveriesInline:
		return &c.DeliveriesInline
	case FDeliveriesViaTool:
		return &c.DeliveriesViaTool
	case FTurnsTriggered:
		return &c.TurnsTriggered
	case FEvictions:
		return &c.Evictions
	case FRewarms:
		return &c.Rewarms
	case FContextTokens:
		return &c.ContextTokens
	case FOversizedTranscripts:
		return &c.OversizedTranscripts
	case FRelaunchesSuppressed:
		return &c.RelaunchesSuppressed
	case FCompactsExpired:
		return &c.CompactsExpired
	case FCompactsDropped:
		return &c.CompactsDropped
	case FCompactsRetried:
		return &c.CompactsRetried
	case FCompactsAccepted:
		return &c.CompactsAccepted
	case FCompactsAbandoned:
		return &c.CompactsAbandoned
	case FTranscriptsTrimmed:
		return &c.TranscriptsTrimmed
	case FTranscriptBytesArchived:
		return &c.TranscriptBytesArchived
	case FTrimsRefused:
		return &c.TrimsRefused
	case FPumpsDeferred:
		return &c.PumpsDeferred
	default:
		return nil
	}
}

// Meter is the per-fleet registry of Counters, one set per bubble address.
// Versioned like inbox.Store so the existing startSaver persistence loop can
// pick it up later without a new mechanism.
type Meter struct {
	mu  sync.Mutex
	ver int64 // bumped on every Add/Set, so a periodic saver can skip idle ticks
	c   map[addr.Address]*Counters
}

// New returns an empty Meter.
func New() *Meter {
	return &Meter{c: make(map[addr.Address]*Counters)}
}

// Version returns a counter that increments on every Add or Set, mirroring
// inbox.Store.Version so callers can reuse the same "only persist on change"
// pattern.
func (m *Meter) Version() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ver
}

// Add accumulates n into bubble a's counter f (e.g. tallying notices written
// over time). Use Set instead for gauge-like fields such as ContextTokens.
func (m *Meter) Add(a addr.Address, f Field, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := field(m.counters(a), f)
	if p == nil {
		return
	}
	*p += n
	m.ver++
}

// Set replaces bubble a's counter f with n outright, for fields that represent
// a current level rather than a running total (e.g. ContextTokens, which
// reflects the model's present context size, not a lifetime sum).
func (m *Meter) Set(a addr.Address, f Field, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := field(m.counters(a), f)
	if p == nil {
		return
	}
	*p = n
	m.ver++
}

// counters returns a's Counters, creating an empty one on first use. Must be
// called with mu held.
func (m *Meter) counters(a addr.Address) *Counters {
	c, ok := m.c[a]
	if !ok {
		c = &Counters{}
		m.c[a] = c
	}
	return c
}

// Snapshot returns a deep copy of every bubble's counters, safe to read after
// the lock is released (e.g. for persistence or a status display) without
// racing further Add/Set calls.
func (m *Meter) Snapshot() map[addr.Address]Counters {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[addr.Address]Counters, len(m.c))
	for a, c := range m.c {
		out[a] = *c
	}
	return out
}
