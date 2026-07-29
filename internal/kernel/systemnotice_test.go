package kernel

import (
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// newNoticeKernel returns a kernel with one hot bubble and its fake session.
func newNoticeKernel(t *testing.T) (*Kernel, addr.Address, *runner.FakeSession) {
	t.Helper()
	k := New(runner.NewFake())
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	s, ok := k.EnsureAlive(a).(*runner.FakeSession)
	if !ok {
		t.Fatal("expected a fake session")
	}
	return k, a, s
}

// TestSystemNoticeIsWrittenAndMetered: a system notice is a direct terminal
// line, not inbox mail -- filing it as mail would cost the recipient an
// inbox() tool call to read a one-line instruction.
func TestSystemNoticeIsWrittenAndMetered(t *testing.T) {
	k, a, s := newNoticeKernel(t)

	if !k.SystemNotice(a, "context is large, please compact()") {
		t.Fatal("SystemNotice reported no write to a hot, ready session")
	}
	if !strings.Contains(s.Written(), "please compact()") {
		t.Fatalf("notice text was not typed, got %q", s.Written())
	}
	if got := k.Cost.Snapshot()[a].NoticesWritten; got != 1 {
		t.Fatalf("NoticesWritten = %d, want 1", got)
	}
	if k.Store.UnreadCount(a) != 0 {
		t.Fatal("a system notice must not be filed as inbox mail")
	}
}

// TestSystemNoticeRespectsTheINV1Ceiling: no path may bypass the flood
// ceiling. The pump calling this on a loop must be bounded like everything
// else -- 632fe95 is the regression this gate exists for.
func TestSystemNoticeRespectsTheINV1Ceiling(t *testing.T) {
	k, a, s := newNoticeKernel(t)

	written := 0
	for i := 0; i < notify.DefaultCeilingBurst+4; i++ {
		if k.SystemNotice(a, "compact() now") {
			written++
		}
	}
	if written > notify.DefaultCeilingBurst {
		t.Fatalf("wrote %d notices, ceiling is %d -- INV-1 must bound the system path too", written, notify.DefaultCeilingBurst)
	}
	if n := strings.Count(s.Written(), "compact() now"); n > notify.DefaultCeilingBurst {
		t.Fatalf("%d lines reached the PTY, ceiling is %d", n, notify.DefaultCeilingBurst)
	}
	if got := k.Cost.Snapshot()[a].NoticesCapped; got == 0 {
		t.Fatal("a capped system notice must be metered; a suppression no counter records is indistinguishable from a lost write")
	}
}

// TestSystemNoticeNeverWakesAColdBubble: the whole point of the pump is to
// save context cost. Paging a bubble in to tell it to compact would pay the
// full prompt-cache rewarm -- self-defeating.
func TestSystemNoticeNeverWakesAColdBubble(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if k.SystemNotice(a, "compact() please") {
		t.Fatal("SystemNotice must report no write for a cold bubble")
	}
	if k.IsHot(a) {
		t.Fatal("SystemNotice must use k.session, never EnsureAlive")
	}
}

// TestSystemNoticeHeldWhileOperatorIsTyping: writing into the focused bubble
// mid-keystroke submits the operator's half-typed line. Every other notice
// path honours the hold; this one must too.
func TestSystemNoticeHeldWhileOperatorIsTyping(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	k.SetFocus(a)
	k.NoteKeystroke()

	if k.SystemNotice(a, "compact() please") {
		t.Fatal("SystemNotice must be held while the operator is typing in the focused bubble")
	}
	if strings.Contains(s.Written(), "compact() please") {
		t.Fatalf("notice was written into a half-typed prompt, got %q", s.Written())
	}
}

// TestSystemNoticeDeferredUntilInputReady: a session still on the resume menu
// swallows an unsubmitted line, so the write must be deferred exactly as the
// mail paths defer it.
func TestSystemNoticeDeferredUntilInputReady(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	s.SetInputReady(false)

	if !k.SystemNotice(a, "compact() please") {
		t.Fatal("SystemNotice must accept the write and defer it, not drop it")
	}
	s.SetInputReady(true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Written(), "compact() please") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deferred notice never landed, got %q", s.Written())
}

// TestSystemNoticeDoesNotDisturbTheAnnouncedBacklog: the announced high-water
// is INV-2 state about MAIL. A system notice announces no mail, so claiming a
// level here would dedup away a real message that arrives afterwards.
func TestSystemNoticeDoesNotDisturbTheAnnouncedBacklog(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	k.SystemNotice(a, "compact() please")
	if got := k.announced(a); got != 0 {
		t.Fatalf("announced = %d, want 0 -- a system notice announces no backlog", got)
	}

	other, err := k.Spawn(addr.Root, "o", "/tmp/o", runner.SpawnOpts{Persona: "o"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	k.Caps.AddContact(other, a)
	if _, err := k.Send(other, a, "real mail", "body", 0, false); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(s.Written(), "real mail") {
		t.Fatalf("a genuine message after a system notice must still be announced, got %q", s.Written())
	}
}

// TestSystemCompactWritesToAReadySession is the happy path: the automated
// compaction path must still actually compact.
func TestSystemCompactWritesToAReadySession(t *testing.T) {
	k, a, s := newNoticeKernel(t)

	if !k.SystemCompact(a, "keep the schema") {
		t.Fatal("SystemCompact reported no write to a hot, ready session")
	}
	if got := s.Written(); !strings.Contains(got, "/compact keep the schema") {
		t.Fatalf("compaction command not typed, got %q", got)
	}
}

// TestSystemCompactHoldsWhileTheOperatorIsTyping is the serious one: the pump
// runs on a 2-minute ticker, and typing /compact plus Enter into the bubble the
// operator is dived into would submit their half-written prompt. That is the
// exact hazard the typing window exists for, and the forced tier used to walk
// straight past it by calling Compact.
func TestSystemCompactHoldsWhileTheOperatorIsTyping(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	k.TypingWindow = time.Hour // treat the operator as continuously typing
	k.SetFocus(a)
	k.NoteKeystroke()

	if k.SystemCompact(a, "") {
		t.Fatal("SystemCompact must refuse while the operator is typing in the focused bubble")
	}
	if strings.Contains(s.Written(), "/compact") {
		t.Fatalf("/compact reached the PTY mid-typing: %q", s.Written())
	}

	// ... and it is retried, not abandoned: the pump only stops asking while
	// the operator is actually typing.
	k.TypingWindow = time.Nanosecond
	if !k.SystemCompact(a, "") {
		t.Fatal("SystemCompact must proceed once the operator has paused")
	}
	if !strings.Contains(s.Written(), "/compact") {
		t.Fatalf("/compact was never written after the pause, got %q", s.Written())
	}
}

// TestSystemCompactRefusesASessionThatIsNotInputReady: a session still booting
// or sitting on the resume menu swallows the line unsubmitted. Reporting false
// (rather than deferring the write) is what keeps the caller's throttle window
// unclaimed for a compaction that never happened.
func TestSystemCompactRefusesASessionThatIsNotInputReady(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	s.SetInputReady(false)

	if k.SystemCompact(a, "") {
		t.Fatal("SystemCompact must report false for a session that is not InputReady")
	}
	if strings.Contains(s.Written(), "/compact") {
		t.Fatalf("/compact must not be typed into a session that would swallow it, got %q", s.Written())
	}

	s.SetInputReady(true)
	if !k.SystemCompact(a, "") {
		t.Fatal("SystemCompact must succeed once the session is ready -- the refusal is a retry, not a giving-up")
	}
}

// TestSystemCompactNeverWakesAColdBubble: compacting a cold bubble would pay a
// full prompt-cache rewarm to save context the bubble is not spending.
func TestSystemCompactNeverWakesAColdBubble(t *testing.T) {
	k := New(runner.NewFake())
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if k.SystemCompact(a, "") {
		t.Fatal("SystemCompact reported a write to a cold bubble")
	}
	if k.IsHot(a) {
		t.Fatal("SystemCompact paged in a cold bubble")
	}
}

// TestInteractiveCompactStillWritesWhileTyping pins the reason SystemCompact is
// a separate entry point rather than a guard inside Compact: the operator's own
// command and a bubble's compact() tool call are deliberate, present-tense
// requests, and must never silently become no-ops because the typing window
// happens to be open.
func TestInteractiveCompactStillWritesWhileTyping(t *testing.T) {
	k, a, s := newNoticeKernel(t)
	k.TypingWindow = time.Hour
	k.SetFocus(a)
	k.NoteKeystroke()

	if err := k.Compact(a, ""); err != nil {
		t.Fatalf("interactive Compact must still work while typing: %v", err)
	}
	if !strings.Contains(s.Written(), "/compact") {
		t.Fatalf("interactive Compact wrote nothing, got %q", s.Written())
	}
}
