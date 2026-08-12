package kernel

import (
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// compactClock is a hand-driven clock, so every deferred-compaction test states
// the passage of time explicitly instead of sleeping through it.
type compactClock struct{ t time.Time }

func (c *compactClock) now() time.Time          { return c.t }
func (c *compactClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// compactKernel returns a kernel with one hot bubble, its fake session, and a
// clock. The session's LastActivity starts AT the clock, i.e. it is producing
// output right now — the state every compact() tool call is made from, since a
// model can only call a tool from inside its own turn.
func compactKernel(t *testing.T) (*Kernel, addr.Address, *runner.FakeSession, *compactClock) {
	t.Helper()
	k, a, s := newNoticeKernel(t)
	c := &compactClock{t: time.Now()}
	k.SetClock(c.now)
	s.SetLastActivity(c.t)
	return k, a, s, c
}

// settled marks the session output-silent for longer than the settle window,
// which is the kernel's turn-end signal.
func settled(s *runner.FakeSession, c *compactClock) {
	s.SetLastActivity(c.t.Add(-CompactSettle - time.Second))
}

// TestCompactWritesNothingAtCallTime is THE regression gate for this bug.
//
// The caller of compact() is a bubble invoking the tool from inside its own
// turn: it is mid-turn by construction and not accepting input, so anything
// typed now is swallowed by the PTY and the compaction silently never happens.
// Compact must therefore record a pending compact and write nothing at all.
func TestCompactWritesNothingAtCallTime(t *testing.T) {
	k, a, s, _ := compactKernel(t)

	if err := k.Compact(a, "keep the schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if w := s.Written(); w != "" {
		t.Fatalf("Compact wrote %q at call time; it must defer until the caller's turn ends", w)
	}
}

// TestPendingCompactIsWrittenOnceOutputGoesSilent: the deferred write actually
// happens — a pending compact that is not eventually typed is the same bug in
// a different disguise.
func TestPendingCompactIsWrittenOnceOutputGoesSilent(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, "keep the schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.FlushPendingCompacts()

	if w := s.Written(); !strings.Contains(w, "/compact keep the schema") {
		t.Fatalf("pending compact was never typed, got %q", w)
	}
	// And it is not typed again on the very next sweep.
	k.FlushPendingCompacts()
	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact typed %d times across two sweeps, want 1", n)
	}
}

// TestPendingCompactHoldsWhileOutputIsFlowing: output inside the settle window
// means the turn is still running, and typing into a running turn is exactly
// what got swallowed before.
func TestPendingCompactHoldsWhileOutputIsFlowing(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	k.FlushPendingCompacts() // LastActivity == now: mid-turn
	if w := s.Written(); w != "" {
		t.Fatalf("pending compact was typed into a session still producing output: %q", w)
	}

	// Still pending, not dropped: once the turn ends it must land.
	settled(s, c)
	k.FlushPendingCompacts()
	if !strings.Contains(s.Written(), "/compact") {
		t.Fatal("a compact held during a turn must still be written once the turn ends")
	}
}

// TestSettleWindowOutlastsAQuietMomentOfRealWork: a short silence is NOT
// turn-end. A bubble that calls compact() and then runs a silent `go build` is
// still mid-turn 30s later, and typing /compact into it is the ORIGINAL BUG —
// swallowed keystrokes. This repo's other output-idle heuristic
// (cmd/bubbles/stuck.go) uses five minutes for a related question, and
// internal/health/stuck.go warns explicitly that LastActivity goes stale during
// "one quiet moment of real work".
func TestSettleWindowOutlastsAQuietMomentOfRealWork(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	s.SetLastActivity(c.t.Add(-30 * time.Second)) // a quiet moment inside a turn
	k.FlushPendingCompacts()

	if w := s.Written(); w != "" {
		t.Fatalf("compact typed after only 30s of silence (%q); the settle window must outlast a quiet moment of real work", w)
	}
}

// TestPendingCompactHoldsWhileTheOperatorIsTyping: submitting /compact into the
// focused bubble's half-typed prompt would send the operator's line.
func TestPendingCompactHoldsWhileTheOperatorIsTyping(t *testing.T) {
	k, a, s, c := compactKernel(t)
	k.TypingWindow = time.Hour
	k.SetFocus(a)
	k.NoteKeystroke()

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.FlushPendingCompacts()
	if w := s.Written(); w != "" {
		t.Fatalf("pending compact was typed into the focused bubble while the operator was typing: %q", w)
	}

	k.TypingWindow = time.Nanosecond // operator paused: the window has closed
	k.FlushPendingCompacts()
	if !strings.Contains(s.Written(), "/compact") {
		t.Fatal("the hold is a delay, not a drop: it must land once typing stops")
	}
}

// TestSevenCompactCallsProduceOneWrite: the reported bubble called compact()
// seven times. Seven `/compact` lines would be seven compactions.
func TestSevenCompactCallsProduceOneWrite(t *testing.T) {
	k, a, s, c := compactKernel(t)

	for i := 0; i < 7; i++ {
		if err := k.Compact(a, "focus"); err != nil {
			t.Fatalf("compact %d: %v", i, err)
		}
	}
	settled(s, c)
	k.FlushPendingCompacts()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("7 compact() calls typed %d /compact lines, want exactly 1 (got %q)", n, s.Written())
	}
}

// TestLastFocusWins: collapsing to one entry must keep the newest instruction.
func TestLastFocusWins(t *testing.T) {
	k, a, s, c := compactKernel(t)

	_ = k.Compact(a, "old focus")
	_ = k.Compact(a, "new focus")
	settled(s, c)
	k.FlushPendingCompacts()

	if w := s.Written(); !strings.Contains(w, "/compact new focus") {
		t.Fatalf("last focus should win, got %q", w)
	}
}

// TestPendingCompactForADeadSessionIsDropped: nothing on a background path may
// wake a cold bubble — a rewarm costs more than the compaction saves.
func TestPendingCompactForADeadSessionIsDropped(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	s.Die()
	launches := len(k.runner.(*runner.FakeRunner).Launches)

	k.FlushPendingCompacts()

	if w := s.Written(); w != "" {
		t.Fatalf("wrote to a dead session: %q", w)
	}
	if got := len(k.runner.(*runner.FakeRunner).Launches); got != launches {
		t.Fatal("the flush relaunched a dead bubble — a background path must never pay a rewarm")
	}
	if k.pendingCompactCount() != 0 {
		t.Fatal("a pending compact for a dead session must be dropped, not retried forever")
	}
}

// TestColdDropIsMetered: EvictIdle pages an idle bubble out and its pending
// compact vanishes with it. Dropping is correct — resurrecting it would pay the
// rewarm this whole programme exists to avoid — but every suppression path in
// this repo increments a counter, or a drop is indistinguishable from a
// compaction the kernel simply lost.
func TestColdDropIsMetered(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.sessions.Delete(a) // paged out, exactly as EvictIdle does

	k.FlushPendingCompacts()

	if k.pendingCompactCount() != 0 {
		t.Fatal("a pending compact for a cold bubble must be dropped")
	}
	if got := k.Cost.Snapshot()[a].CompactsDropped; got != 1 {
		t.Fatalf("CompactsDropped = %d, want 1 — a silent drop is indistinguishable from a lost compaction", got)
	}
}

// TestPendingCompactExpiryIsMetered: a compact that silently never happens is
// the bug being fixed, so the give-up is counted rather than invisible.
func TestPendingCompactExpiryIsMetered(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	// The session never stops producing output, so the entry never becomes
	// flushable; time runs past the bound.
	c.advance(CompactExpiry + time.Second)
	s.SetLastActivity(c.t)
	k.FlushPendingCompacts()

	if w := s.Written(); w != "" {
		t.Fatalf("an expired compact must not be written, got %q", w)
	}
	if k.pendingCompactCount() != 0 {
		t.Fatal("an expired pending compact must be dropped")
	}
	if got := k.Cost.Snapshot()[a].CompactsExpired; got != 1 {
		t.Fatalf("CompactsExpired = %d, want 1 — a compact that never runs must be metered", got)
	}
}

// TestASwallowedWriteIsRetriedAndMetered: a successful s.Write is NOT a
// successful compaction. If the session produces no output whatsoever after the
// command is typed, nothing received it — that is the swallow this fix exists
// to eliminate, and it must be re-issued rather than assumed to have worked.
func TestASwallowedWriteIsRetriedAndMetered(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.FlushPendingCompacts()
	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("first write: /compact typed %d times, want 1", n)
	}

	// The session says nothing at all in response: the line was swallowed. One
	// sweep judges it swallowed and re-queues it; the next re-types it (the
	// entry moves one step per sweep, and the sweep runs every 2s).
	c.advance(compactReactWindow + time.Second)
	k.FlushPendingCompacts()
	k.FlushPendingCompacts()

	if n := strings.Count(s.Written(), "/compact"); n != 2 {
		t.Fatalf("a swallowed /compact was not re-issued: typed %d times, want 2", n)
	}
	if got := k.Cost.Snapshot()[a].CompactsRetried; got != 1 {
		t.Fatalf("CompactsRetried = %d, want 1 — a re-issued compact must be metered", got)
	}
}

// TestSwallowedWriteRetriesAreBoundedAndTheGiveUpIsMetered: retrying forever is
// its own failure mode, and giving up silently is the original bug wearing a
// hat.
func TestSwallowedWriteRetriesAreBoundedAndTheGiveUpIsMetered(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	for i := 0; i < maxCompactWrites+3; i++ {
		k.FlushPendingCompacts() // verify the last write / re-queue it
		k.FlushPendingCompacts() // type it again
		c.advance(compactReactWindow + time.Second)
		// LastActivity is deliberately NOT refreshed: the session stays silent,
		// which is what "every write was swallowed" means.
	}

	if n := strings.Count(s.Written(), "/compact"); n != maxCompactWrites {
		t.Fatalf("/compact typed %d times, want exactly %d", n, maxCompactWrites)
	}
	if k.pendingCompactCount() != 0 {
		t.Fatal("an abandoned compact must not stay pending forever")
	}
	if got := k.Cost.Snapshot()[a].CompactsAbandoned; got != 1 {
		t.Fatalf("CompactsAbandoned = %d, want 1 — giving up must be metered", got)
	}
}

// TestAWriteTheSessionRespondsToIsAccepted: output after the command means the
// session received it, so it must not be typed a second time. Re-compacting a
// bubble that already compacted costs a full extra summarization pass.
func TestAWriteTheSessionRespondsToIsAccepted(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.FlushPendingCompacts()

	c.advance(compactReactWindow + time.Second)
	s.SetLastActivity(c.t) // the compaction turn is talking

	k.FlushPendingCompacts()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact typed %d times after the session responded, want 1", n)
	}
	if k.pendingCompactCount() != 0 {
		t.Fatal("an accepted compact must be dropped once the session responds")
	}
	if got := k.Cost.Snapshot()[a].CompactsRetried; got != 0 {
		t.Fatalf("CompactsRetried = %d, want 0 — a compaction that landed must not be re-issued", got)
	}
}

// TestPendingCompactFocusIsSanitised: the focus string comes from a bubble and
// is typed into a session. Unsanitised it is a keystroke-injection path.
func TestPendingCompactFocusIsSanitised(t *testing.T) {
	k, a, s, c := compactKernel(t)

	if err := k.Compact(a, "keep\r\nthe /exit schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s, c)
	k.FlushPendingCompacts()

	w := s.Written()
	if strings.ContainsAny(strings.TrimSuffix(w, "\r"), "\r\n") {
		t.Fatalf("newlines survived into the typed line: %q", w)
	}
	if !strings.HasPrefix(w, "/compact ") {
		t.Fatalf("the typed line must still be a /compact command, got %q", w)
	}
}

// TestCompactOnAColdBubbleErrors: the caller is told, rather than being
// promised a compaction that can never run.
func TestCompactOnAColdBubbleErrors(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "", "/tmp/a", runner.SpawnOpts{Name: "a"})

	if err := k.Compact(a, "x"); err == nil {
		t.Fatal("compacting a cold bubble should error")
	}
	if k.pendingCompactCount() != 0 {
		t.Fatal("a cold bubble must not leave a pending compact behind")
	}
}
