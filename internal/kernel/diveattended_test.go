package kernel

import (
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func newAttendKernel() *Kernel { return New(runner.NewFake()) }

// TestDiveAttendedWithoutAnyKeystroke is the discriminating test. An operator
// who dives in to READ never presses a key, and SetFocus zeroes the keystroke
// clock — so the old isFocused && typingActive guard reported "nobody here" and
// background writes landed on the screen mid-conversation.
func TestDiveAttendedWithoutAnyKeystroke(t *testing.T) {
	k := newAttendKernel()
	const a addr.Address = "0.6"
	k.SetFocus(a)
	if !k.DiveAttended(a) {
		t.Fatal("a dive with no keystrokes must count as attended; this is the read-only operator")
	}
	if k.typingActive() {
		t.Fatal("precondition failed: SetFocus must leave typing inactive, or this test proves nothing")
	}
}

// TestDiveAttendedOnlyTheFocusedBubble: deferring must be narrow. Every other
// bubble in the fleet still gets pumped while the operator sits in one of them.
func TestDiveAttendedOnlyTheFocusedBubble(t *testing.T) {
	k := newAttendKernel()
	k.SetFocus("0.6")
	if k.DiveAttended("0.2") {
		t.Fatal("a bubble the operator is NOT in must not be treated as attended")
	}
	if k.DiveAttended("") {
		t.Fatal("the empty address must never be attended")
	}
}

// TestDiveAttendedAgesOut: the stale-focus case (3cfb0d9). A terminal that drops
// mid-dive never runs UnsetFocus, so focus stays set forever. Attendance must
// expire on its own or the bubble would be exempt from the pump for good.
func TestDiveAttendedAgesOut(t *testing.T) {
	k := newAttendKernel()
	const a addr.Address = "0.6"
	k.SetFocus(a)

	k.focusMu.Lock()
	k.focusedAt = time.Now().Add(-presenceWindow - time.Minute)
	k.focusMu.Unlock()

	if k.DiveAttended(a) {
		t.Fatal("a focus older than presenceWindow with no keystrokes must age out, or a dropped terminal strands the bubble")
	}
}

// TestDiveAttendedKeystrokeRefreshes: a long dive stays attended as long as the
// operator is actually interacting, past the entry window.
func TestDiveAttendedKeystrokeRefreshes(t *testing.T) {
	k := newAttendKernel()
	const a addr.Address = "0.6"
	k.SetFocus(a)
	k.focusMu.Lock()
	k.focusedAt = time.Now().Add(-presenceWindow - time.Minute)
	k.focusMu.Unlock()

	k.NoteKeystroke()
	if !k.DiveAttended(a) {
		t.Fatal("a recent keystroke must keep a long dive attended")
	}
}

// TestDiveAttendedFalseAfterLeaving: leaving must re-enable the pump at once.
func TestDiveAttendedFalseAfterLeaving(t *testing.T) {
	k := newAttendKernel()
	const a addr.Address = "0.6"
	k.SetFocus(a)
	k.UnsetFocus(a)
	if k.DiveAttended(a) {
		t.Fatal("after UnsetFocus the bubble must be pumpable again immediately")
	}
}
