package kernel

import (
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// compactKernel returns a kernel with one hot bubble, its fake session, and a
// LastActivity stamp that is still INSIDE the settle window — i.e. the session
// looks like it is mid-turn, which is the state every compact() tool call is
// made from.
func compactKernel(t *testing.T) (*Kernel, addr.Address, *runner.FakeSession) {
	t.Helper()
	k, a, s := newNoticeKernel(t)
	s.SetLastActivity(time.Now()) // producing output right now: mid-turn
	return k, a, s
}

// busy marks the session as still producing output (turn in progress).
func busy(s *runner.FakeSession) { s.SetLastActivity(time.Now()) }

// settled marks the session as output-silent for longer than the settle window,
// which is the kernel's turn-end signal.
func settled(s *runner.FakeSession) {
	s.SetLastActivity(time.Now().Add(-2 * CompactSettle))
}

// TestCompactWritesNothingAtCallTime is THE regression gate for this bug.
//
// The caller of compact() is a bubble invoking the tool from inside its own
// turn: it is mid-turn by construction and not accepting input, so anything
// typed now is swallowed by the PTY and the compaction silently never happens.
// Compact must therefore record a pending compact and write nothing at all.
func TestCompactWritesNothingAtCallTime(t *testing.T) {
	k, a, s := compactKernel(t)

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
	k, a, s := compactKernel(t)

	if err := k.Compact(a, "keep the schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s)
	k.FlushPendingCompacts()

	if w := s.Written(); !strings.Contains(w, "/compact keep the schema") {
		t.Fatalf("pending compact was never typed, got %q", w)
	}
	// And it is gone: a flushed entry must not be typed again on the next sweep.
	k.FlushPendingCompacts()
	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("/compact typed %d times across two sweeps, want 1", n)
	}
}

// TestPendingCompactHoldsWhileOutputIsFlowing: output inside the settle window
// means the turn is still running, and typing into a running turn is exactly
// what got swallowed before.
func TestPendingCompactHoldsWhileOutputIsFlowing(t *testing.T) {
	k, a, s := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	busy(s)
	k.FlushPendingCompacts()
	if w := s.Written(); w != "" {
		t.Fatalf("pending compact was typed into a session still producing output: %q", w)
	}

	// Still pending, not dropped: once the turn ends it must land.
	settled(s)
	k.FlushPendingCompacts()
	if !strings.Contains(s.Written(), "/compact") {
		t.Fatal("a compact held during a turn must still be written once the turn ends")
	}
}

// TestPendingCompactHoldsWhileTheOperatorIsTyping: submitting /compact into the
// focused bubble's half-typed prompt would send the operator's line.
func TestPendingCompactHoldsWhileTheOperatorIsTyping(t *testing.T) {
	k, a, s := compactKernel(t)
	k.TypingWindow = time.Hour
	k.SetFocus(a)
	k.NoteKeystroke()

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s)
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
	k, a, s := compactKernel(t)

	for i := 0; i < 7; i++ {
		if err := k.Compact(a, "focus"); err != nil {
			t.Fatalf("compact %d: %v", i, err)
		}
	}
	settled(s)
	k.FlushPendingCompacts()

	if n := strings.Count(s.Written(), "/compact"); n != 1 {
		t.Fatalf("7 compact() calls typed %d /compact lines, want exactly 1 (got %q)", n, s.Written())
	}
}

// TestLastFocusWins: collapsing to one entry must keep the newest instruction.
func TestLastFocusWins(t *testing.T) {
	k, a, s := compactKernel(t)

	_ = k.Compact(a, "old focus")
	_ = k.Compact(a, "new focus")
	settled(s)
	k.FlushPendingCompacts()

	if w := s.Written(); !strings.Contains(w, "/compact new focus") {
		t.Fatalf("last focus should win, got %q", w)
	}
}

// TestPendingCompactForADeadSessionIsDropped: nothing on a background path may
// wake a cold bubble — a rewarm costs more than the compaction saves.
func TestPendingCompactForADeadSessionIsDropped(t *testing.T) {
	k, a, s := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s)
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

// TestPendingCompactExpiryIsMetered: a compact that silently never happens is
// the bug being fixed, so the give-up is counted rather than invisible.
func TestPendingCompactExpiryIsMetered(t *testing.T) {
	k, a, s := compactKernel(t)

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	busy(s) // never settles

	// Age the pending entry past its bound.
	k.agePendingCompacts(CompactExpiry + time.Second)
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

// TestPendingCompactFocusIsSanitised: the focus string comes from a bubble and
// is typed into a session. Unsanitised it is a keystroke-injection path.
func TestPendingCompactFocusIsSanitised(t *testing.T) {
	k, a, s := compactKernel(t)

	if err := k.Compact(a, "keep\r\nthe /exit schema"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	settled(s)
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
