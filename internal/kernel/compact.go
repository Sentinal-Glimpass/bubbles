package kernel

import (
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
)

// Deferred compaction.
//
// `compact()` is called by a bubble from INSIDE its own turn — that is the only
// place a model can call a tool from. A session mid-turn is not accepting input,
// so a `/compact` typed at that moment is swallowed by the PTY and the
// compaction silently never happens, while the tool reply promises it will. That
// was measured in production: one bubble made 7+ calls and stayed pinned at
// 792k, another went 505k -> 581k across 7 calls, and every later turn then
// billed the full context.
//
// So Compact records a PENDING compact and returns; FlushPendingCompacts (a
// supervisor check) types it once the caller's turn has actually ended, and then
// KEEPS the entry until the session proves it received the command.
//
// TURN-END SIGNAL: Session.LastActivity(), the last *output*, the same signal
// internal/health uses for stuck detection. InputReady is deliberately NOT used:
// it is a one-way latch (readyWatcher only ever stores true, including on
// boot-deadline timeout and on process death), so it means "was ready once",
// never "is ready now", and it cannot detect the end of a turn.
//
// SystemCompact (notify.go) is a SEPARATE, unchanged mechanism: the Phase 2
// context pump calls it from a sweep at a moment the bubble is already idle, so
// its inline write is correct there and stays as it is.
const (
	// CompactSettle is how long a session must be output-silent before its
	// pending compact is typed.
	//
	// The number is chosen to outlast a QUIET MOMENT OF REAL WORK, not merely a
	// gap between tokens. A bubble that calls compact() and then shells out to a
	// 60-second `go build` is still mid-turn and still not accepting input, so a
	// command typed into that silence is swallowed exactly as before the fix —
	// the residual form of the original bug. internal/health/stuck.go names this
	// failure directly ("output can be unchanged simply because two samples
	// landed inside one quiet moment of real work") and refuses to rely on
	// LastActivity alone because of it.
	//
	// This repo's other output-idle heuristic, cmd/bubbles/stuck.go, uses FIVE
	// MINUTES — but it answers a different question ("is this bubble wedged?"),
	// where the cost of being wrong is an operator disturbing working bubble, so
	// it can afford to be that conservative. Here the cost of waiting is a
	// compaction landing one checkpoint later than it could have, and the cost of
	// being wrong is a swallowed write, which is now DETECTED and retried (see
	// compactReactWindow) rather than lost. 60s buys most of the safety of five
	// minutes without deferring the compaction across several billed turns.
	CompactSettle = 60 * time.Second

	// compactReactWindow is how long a written `/compact` has to provoke ANY
	// output before the write is judged swallowed.
	//
	// This exists because a successful s.Write is NOT a successful compaction: it
	// means bytes reached the PTY, not that the session was in a state to act on
	// them. A session that received the command starts a compaction turn and
	// therefore produces output; a session that swallowed it says nothing at all.
	// So "no output since the write" is the falsification signal.
	//
	// The check is deliberately biased toward believing the write LANDED (any
	// output at all, including a mere echo of the typed characters, counts as
	// receipt). That direction is the safe one: a false "landed" costs one missed
	// compaction, which the pump will ask for again, whereas a false "swallowed"
	// costs a redundant full summarization pass on a bubble that already
	// compacted — real money, in the currency this whole programme is spending
	// itself to save.
	compactReactWindow = 45 * time.Second

	// maxCompactWrites bounds how many times one queued compaction may be typed.
	// Retrying forever is its own failure mode; giving up is metered.
	maxCompactWrites = 3

	// CompactExpiry bounds how long a pending compact may wait for a flushable
	// moment. A bubble that never goes quiet (or whose operator never stops
	// typing) must not accumulate an entry that is retried forever; giving up is
	// metered, because a compaction that silently never happens is precisely the
	// bug this file exists to fix.
	CompactExpiry = 30 * time.Minute
)

// WHY NOT VERIFY WITH ContextTokens.
//
// The obvious verification is "did the context actually shrink?", reading the
// costmeter's ContextTokens gauge that the Phase 2 pump publishes. It was
// rejected, because on this data it cannot distinguish success from failure:
//
//   - The gauge is written only by cmd/bubbles' health sweep, on a 2-minute
//     cadence, and costmeter stores bare int64s with no sample timestamp. There
//     is therefore no way to tell "sampled since the write and unchanged" from
//     "not sampled yet" — the exact ambiguity that makes a retry loop fire on
//     no evidence.
//   - Worse, transcript.Read takes ContextTokens from the LAST usage-bearing
//     entry, and the compaction turn itself is billed on the FULL pre-compaction
//     context. Immediately after a successful compaction the gauge therefore
//     reads unchanged or higher until the bubble takes another real turn. A
//     shrink test would read a successful compaction as a failure and re-issue
//     `/compact` against an already-compacted conversation, spending a second
//     summarization pass to fix nothing.
//
// The session's own output is used instead: it is kernel-owned, always fresh,
// needs no cross-layer gauge, and its error direction is the safe one.

// compactState is where a queued compaction is in its life.
type compactState int

const (
	// compactQueued: recorded, not yet typed.
	compactQueued compactState = iota
	// compactWritten: typed, waiting for the session to prove it received it.
	compactWritten
)

// pendingCompact is one bubble's queued compaction. Repeated compact() calls
// for one address collapse onto a single entry (last focus wins), so a bubble
// that calls it seven times is compacted once, not seven times.
type pendingCompact struct {
	focus string
	at    time.Time    // when it was queued; drives expiry and the flush-time identity check
	state compactState // queued -> written -> (accepted: dropped | swallowed: queued again)
	wrote time.Time    // when the command was last typed (state == compactWritten)
	quiet time.Time    // the session's LastActivity at that moment: output past this is receipt
	tries int          // how many times it has been typed; bounded by maxCompactWrites
}

// queueCompact records (or replaces) a's pending compact. Its own mutex, held
// only for the map write and NEVER across a PTY write.
//
// A fresh call REPLACES an entry that is mid-verification, deliberately: it is a
// new request made at a new checkpoint, and the bubble asking again is the
// bubble telling us the last one did not do what it wanted.
func (k *Kernel) queueCompact(a addr.Address, focus string, at time.Time) {
	k.compactMu.Lock()
	k.pendingCompacts[a] = pendingCompact{focus: focus, at: at, state: compactQueued}
	k.compactMu.Unlock()
}

// sameEntry reports whether cur is still the entry the flush observed. Anything
// that changed it (a fresh compact() call, another mutation) wins over the
// stale view the flush is holding.
func sameEntry(cur, p pendingCompact) bool {
	return cur.at.Equal(p.at) && cur.state == p.state && cur.tries == p.tries
}

// dropCompact removes a's pending entry, but only if it is still the one the
// caller observed.
func (k *Kernel) dropCompact(a addr.Address, p pendingCompact) {
	k.compactMu.Lock()
	if cur, ok := k.pendingCompacts[a]; ok && sameEntry(cur, p) {
		delete(k.pendingCompacts, a)
	}
	k.compactMu.Unlock()
}

// updateCompact replaces a's entry with next, under the same identity check.
func (k *Kernel) updateCompact(a addr.Address, p, next pendingCompact) {
	k.compactMu.Lock()
	if cur, ok := k.pendingCompacts[a]; ok && sameEntry(cur, p) {
		k.pendingCompacts[a] = next
	}
	k.compactMu.Unlock()
}

// pendingCompactCount reports how many compactions are outstanding (tests, and
// a cheap way to keep the flush free of work when nothing is pending).
func (k *Kernel) pendingCompactCount() int {
	k.compactMu.Lock()
	defer k.compactMu.Unlock()
	return len(k.pendingCompacts)
}

// FlushPendingCompacts drives every outstanding compaction one step: it types
// the queued `/compact` once the session's turn has ended, and then checks that
// the session reacted to it. It is the supervisor check "compact-flush".
//
// Every guard must hold before a write:
//   - the session is alive (and it is looked up with k.session, NEVER
//     EnsureAlive: a pending compact for a COLD bubble is DROPPED, never used to
//     wake it — a rewarm costs far more than the compaction saves, and avoiding
//     rewarms is the entire point of this programme);
//   - it is not the focused bubble while the operator is typing, so a background
//     sweep can never submit the operator's half-typed line;
//   - it has been output-silent for CompactSettle, i.e. its turn has ended.
//
// A guard that does not hold is a DELAY, not a drop: the entry stays until it
// flushes or expires. EVERY path that removes an entry without a compaction
// having demonstrably happened increments a counter — cold/dead drop, expiry,
// and abandonment after maxCompactWrites — because a compaction that silently
// never happens is the bug this file exists to fix, and a silent drop is
// indistinguishable from it.
//
// The map lock is taken only to snapshot, update and drop; it is never held
// across a PTY write.
func (k *Kernel) FlushPendingCompacts() {
	k.compactMu.Lock()
	if len(k.pendingCompacts) == 0 {
		k.compactMu.Unlock()
		return
	}
	snapshot := make(map[addr.Address]pendingCompact, len(k.pendingCompacts))
	for a, p := range k.pendingCompacts {
		snapshot[a] = p
	}
	k.compactMu.Unlock()

	now := k.now()
	for a, p := range snapshot {
		s := k.session(a)
		if s == nil || !s.Alive() {
			// Cold (paged out by EvictIdle) or dead. Dropping is correct — waking it
			// to compact would pay the rewarm — but it is counted, not silent.
			k.dropCompact(a, p)
			k.Cost.Add(a, costmeter.FCompactsDropped, 1)
			continue
		}
		if p.state == compactWritten {
			k.verifyCompact(a, p, s.LastActivity(), now)
			continue
		}
		if k.isFocused(a) && k.typingActive() {
			k.expireCompact(a, p, now)
			continue
		}
		last := s.LastActivity()
		if last.IsZero() || now.Sub(last) < CompactSettle {
			k.expireCompact(a, p, now) // still producing output: the turn is not over
			continue
		}
		// compactCommand sanitises the focus: it comes from a bubble and is typed
		// into a session, so unsanitised it is a keystroke-injection path.
		if _, err := s.Write([]byte(compactCommand(p.focus))); err != nil { // Write appends Enter
			k.expireCompact(a, p, now) // failed write: retry next sweep, within the bound
			continue
		}
		next := p
		next.state, next.wrote, next.quiet, next.tries = compactWritten, now, last, p.tries+1
		k.updateCompact(a, p, next)
	}
}

// verifyCompact decides what a written `/compact` actually achieved.
//
// Output produced after the moment it was typed means the session reacted, and
// therefore received it: the entry is done. No output at all once
// compactReactWindow has passed means nothing consumed the line — the swallow —
// so it is queued again, up to maxCompactWrites. Both the re-issue and the final
// give-up are metered.
func (k *Kernel) verifyCompact(a addr.Address, p pendingCompact, last, now time.Time) {
	if last.After(p.quiet) {
		k.dropCompact(a, p) // the session is talking: the command landed
		k.Cost.Add(a, costmeter.FCompactsAccepted, 1)
		return
	}
	if now.Sub(p.wrote) < compactReactWindow {
		return // too early to call it
	}
	if p.tries >= maxCompactWrites {
		k.dropCompact(a, p)
		k.Cost.Add(a, costmeter.FCompactsAbandoned, 1)
		return
	}
	next := p
	next.state = compactQueued // back to the front of the queue, guards and all
	k.updateCompact(a, p, next)
	k.Cost.Add(a, costmeter.FCompactsRetried, 1)
}

// expireCompact drops a pending entry that has waited past CompactExpiry and
// records the give-up. Anything younger is left alone to try again next sweep.
func (k *Kernel) expireCompact(a addr.Address, p pendingCompact, now time.Time) {
	if now.Sub(p.at) < CompactExpiry {
		return
	}
	k.dropCompact(a, p)
	k.Cost.Add(a, costmeter.FCompactsExpired, 1)
}
