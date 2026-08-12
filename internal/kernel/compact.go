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
// supervisor check) types it once the caller's turn has actually ended.
//
// TURN-END SIGNAL: LastActivity(), the session's last *output*, the same signal
// internal/health uses for stuck detection. A session that has produced no
// output for CompactSettle has finished its turn. InputReady is deliberately NOT
// used: it is a one-way latch (readyWatcher only ever stores true, including on
// boot-deadline timeout and on process death), so it means "was ready once",
// never "is ready now", and it cannot detect the end of a turn.
//
// SystemCompact (notify.go) is a SEPARATE, unchanged mechanism: the Phase 2
// context pump calls it from a sweep at a moment the bubble is already idle, so
// its inline write is correct there and stays as it is.
const (
	// CompactSettle is how long a session must be output-silent before its
	// pending compact is typed. Long enough that a pause inside a turn (a tool
	// call thinking, a slow token) does not read as the end of one; short enough
	// that the compaction lands on the next natural checkpoint rather than
	// several turns later, by which time the context it was meant to reclaim has
	// already been billed again.
	CompactSettle = 10 * time.Second

	// CompactExpiry bounds how long a pending compact may wait for a flushable
	// moment. A bubble that never goes quiet (or whose operator never stops
	// typing) must not accumulate an entry that is retried forever; giving up is
	// metered, because a compaction that silently never happens is precisely the
	// bug this file exists to fix.
	CompactExpiry = 30 * time.Minute
)

// pendingCompact is one bubble's queued compaction. Repeated compact() calls
// for one address collapse onto a single entry (last focus wins), so a bubble
// that calls it seven times is compacted once, not seven times.
type pendingCompact struct {
	focus string
	at    time.Time // when it was queued; drives expiry and the flush-time identity check
}

// queueCompact records (or replaces) a's pending compact. Its own mutex, held
// only for the map write and NEVER across a PTY write.
func (k *Kernel) queueCompact(a addr.Address, focus string, at time.Time) {
	k.compactMu.Lock()
	k.pendingCompacts[a] = pendingCompact{focus: focus, at: at}
	k.compactMu.Unlock()
}

// dropCompact removes a's pending entry, but only if it is still the one the
// caller observed. A compact() call that arrived while the flush was writing
// leaves a newer entry with a later stamp, and that one must survive.
func (k *Kernel) dropCompact(a addr.Address, p pendingCompact) {
	k.compactMu.Lock()
	if cur, ok := k.pendingCompacts[a]; ok && cur.at.Equal(p.at) {
		delete(k.pendingCompacts, a)
	}
	k.compactMu.Unlock()
}

// pendingCompactCount reports how many compactions are queued (tests, and a
// cheap way to keep the flush free of work when nothing is pending).
func (k *Kernel) pendingCompactCount() int {
	k.compactMu.Lock()
	defer k.compactMu.Unlock()
	return len(k.pendingCompacts)
}

// agePendingCompacts moves every pending entry d further into the past, so an
// expiry test does not have to sleep for CompactExpiry.
func (k *Kernel) agePendingCompacts(d time.Duration) {
	k.compactMu.Lock()
	for a, p := range k.pendingCompacts {
		p.at = p.at.Add(-d)
		k.pendingCompacts[a] = p
	}
	k.compactMu.Unlock()
}

// FlushPendingCompacts types each pending `/compact` into the session it was
// queued for, once that session's turn has ended. It is the supervisor check
// registered as "compact-flush".
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
// A guard that does not hold is a DELAY, not a drop: the entry stays pending
// until it flushes or expires. Only death (drop, silently — there is nothing to
// write to) and expiry (drop, metered) remove an entry unwritten.
//
// The map lock is taken only to snapshot and to drop; it is never held across
// a PTY write.
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
			k.dropCompact(a, p) // cold or dead: nothing to write to, and never worth a rewarm
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
		k.dropCompact(a, p)
	}
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
