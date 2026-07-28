package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestMutedMessageSuppressesWakeAndNotice(t *testing.T) {
	rs := NewRuleSet()
	_ = rs.Add(Rule{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour})
	p := NewPolicy(func(addr.Address) *RuleSet { return rs }, NewCeiling(6, 6))
	now := time.Unix(0, 0)

	// First match opens the window: delivered normally.
	d1 := p.Decide("0.1", Message{ID: 1, Source: "pump", Subject: "opt_out", Urgent: true}, State{Notifiable: 1}, now)
	if d1.Action == Suppress {
		t.Fatal("first match must deliver and open the window")
	}
	// Inside the window: suppressed, and crucially NOT allowed to wake.
	d2 := p.Decide("0.1", Message{ID: 2, Source: "pump", Subject: "opt_out", Urgent: true}, State{Notifiable: 2}, now.Add(time.Minute))
	if d2.Action != Suppress {
		t.Fatalf("Action = %v, want Suppress", d2.Action)
	}
	if d2.Wake {
		t.Fatal("muted message must NOT wake a cold bubble even when urgent")
	}
	if !d2.MarkMuted {
		t.Fatal("suppressed message must be marked muted so it is not counted as notifiable")
	}
	if d2.Rule != "r1" {
		t.Fatalf("Rule = %q, want r1", d2.Rule)
	}
}

func TestWindowExpiryProducesRollup(t *testing.T) {
	rs := NewRuleSet()
	_ = rs.Add(Rule{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour})
	p := NewPolicy(func(addr.Address) *RuleSet { return rs }, NewCeiling(6, 6))
	now := time.Unix(0, 0)

	p.Decide("0.1", Message{ID: 1, Source: "pump", Subject: "opt_out"}, State{Notifiable: 1}, now)
	for i := 2; i <= 15; i++ {
		p.Decide("0.1", Message{ID: i, Source: "pump", Subject: "opt_out"}, State{Notifiable: i}, now.Add(time.Minute))
	}
	d := p.Decide("0.1", Message{ID: 16, Source: "pump", Subject: "opt_out"}, State{Notifiable: 16}, now.Add(2*time.Hour))
	if d.Action != Rollup {
		t.Fatalf("Action = %v, want Rollup after window expiry", d.Action)
	}
	if !strings.Contains(d.Text, "opt_out") {
		t.Fatalf("rollup text must name the subject, got %q", d.Text)
	}
}

func TestSmallBodyIsInlined(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	d := p.Decide("0.1", Message{ID: 7, Source: "0.2", Subject: "status", Body: "build green"},
		State{Notifiable: 1, Hot: true}, time.Unix(0, 0))
	if d.Action != Inline {
		t.Fatalf("Action = %v, want Inline for a short body", d.Action)
	}
	if !strings.Contains(d.Text, "build green") {
		t.Fatalf("inlined text must carry the body, got %q", d.Text)
	}
	if len(d.MarkRead) != 1 || d.MarkRead[0] != 7 {
		t.Fatalf("MarkRead = %v, want [7]", d.MarkRead)
	}
}

func TestLargeBodyFallsBackToNotice(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	big := strings.Repeat("x", InlineMaxBytes+1)
	d := p.Decide("0.1", Message{ID: 7, Source: "0.2", Subject: "dump", Body: big},
		State{Notifiable: 1, Hot: true}, time.Unix(0, 0))
	if d.Action != Notice {
		t.Fatalf("Action = %v, want Notice for an oversized body", d.Action)
	}
	if len(d.MarkRead) != 0 {
		t.Fatal("a non-inlined message must not be marked read")
	}
}

func TestInlinedTextIsSingleLineAndSanitised(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	d := p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s", Body: "line1\nline2\x1b[31m\rX"},
		State{Notifiable: 1, Hot: true}, time.Unix(0, 0))
	if strings.ContainsAny(d.Text, "\n\r\x1b") {
		t.Fatalf("inlined text must be single-line and escape-free, got %q", d.Text)
	}
}

func TestCeilingOverridesPolicy(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	now := time.Unix(0, 0)
	written := 0
	// Urgent, so every message bypasses coalescing and the ceiling is the
	// only thing standing between 178 arrivals and 178 writes -- which is
	// exactly the 632fe95 shape.
	for i := 1; i <= 178; i++ {
		d := p.Decide("0.1", Message{ID: i, Source: "0.2", Subject: "s", Urgent: true}, State{Notifiable: i}, now)
		if d.Action != Suppress {
			written++
		}
	}
	if written != DefaultCeilingBurst {
		t.Fatalf("written = %d, want exactly the INV-1 ceiling %d", written, DefaultCeilingBurst)
	}
}

// INV-2, direction A: a backlog that survives a relaunch is still announced.
func TestBacklogSurvivingRelaunchIsAnnounced(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	now := time.Unix(0, 0)
	d := p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s"}, State{Notifiable: 3, Announced: 0}, now)
	if d.Action == Suppress {
		t.Fatal("an unannounced backlog must be announced (silent-stall direction)")
	}
}

// INV-2, direction B: an already-announced backlog does not re-announce.
func TestAnnouncedBacklogDoesNotReannounce(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	now := time.Unix(0, 0)
	d := p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s"}, State{Notifiable: 3, Announced: 3}, now)
	if d.Action != Suppress {
		t.Fatalf("Action = %v, want Suppress -- re-announcing is the 632fe95 flood direction", d.Action)
	}
}

func TestPendingIsEmptyWithNoCoalesceWindow(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	if _, ok := p.Pending("0.1", time.Unix(0, 0)); ok {
		t.Fatal("Pending must report nothing for a bubble that never received a message")
	}
	// A window that is open but has swallowed nothing is also nothing to say.
	p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s"}, State{Notifiable: 1}, time.Unix(0, 0))
	if _, ok := p.Pending("0.1", time.Unix(0, 0).Add(2*CoalesceWindow)); ok {
		t.Fatal("Pending must report nothing when the window buffered no followers")
	}
}

func TestPendingDrainsBufferedIDsAfterWindowExpiry(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(6, 6))
	now := time.Unix(0, 0)

	// Message 1 delivers and opens the window; 2..5 are buffered inside it.
	p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s"}, State{Notifiable: 1}, now)
	for i := 2; i <= 5; i++ {
		d := p.Decide("0.1", Message{ID: i, Source: "0.2", Subject: "s"}, State{Notifiable: i}, now.Add(time.Second))
		if d.Action != Suppress {
			t.Fatalf("message %d: Action = %v, want Suppress inside the coalesce window", i, d.Action)
		}
	}
	if _, ok := p.Pending("0.1", now.Add(time.Second)); ok {
		t.Fatal("Pending must not drain a window that is still open")
	}

	d, ok := p.Pending("0.1", now.Add(2*CoalesceWindow))
	if !ok {
		t.Fatal("Pending must drain an expired window that buffered followers")
	}
	if d.Action == Suppress {
		t.Fatalf("Action = %v, want a written action", d.Action)
	}
	if len(d.IDs) != 4 {
		t.Fatalf("IDs = %v, want the 4 buffered followers", d.IDs)
	}
	for i, id := range d.IDs {
		if id != i+2 {
			t.Fatalf("IDs = %v, want [2 3 4 5]", d.IDs)
		}
	}
	// The buffer is consumed: a second drain has nothing left.
	if _, ok := p.Pending("0.1", now.Add(2*CoalesceWindow)); ok {
		t.Fatal("Pending must not re-emit an already-drained batch")
	}
}

func TestPendingRespectsCeiling(t *testing.T) {
	// Burst of 1: the opening message spends the only token, so the drain
	// must be capped rather than treated as a ceiling bypass.
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(0, 1))
	now := time.Unix(0, 0)

	p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s"}, State{Notifiable: 1}, now)
	p.Decide("0.1", Message{ID: 2, Source: "0.2", Subject: "s"}, State{Notifiable: 2}, now.Add(time.Second))
	p.Decide("0.1", Message{ID: 3, Source: "0.2", Subject: "s"}, State{Notifiable: 3}, now.Add(time.Second))

	if _, ok := p.Pending("0.1", now.Add(2*CoalesceWindow)); ok {
		t.Fatal("Pending must not write past the INV-1 ceiling")
	}
}

// A capped rollup must not destroy the batch it failed to report: the
// swallowed messages are marked muted, so the window's count is the only
// remaining record of them.
func TestCappedMuteRollupPreservesCount(t *testing.T) {
	rs := NewRuleSet()
	_ = rs.Add(Rule{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour})
	// 2 tokens, refilling at 0.1/s.
	p := NewPolicy(func(addr.Address) *RuleSet { return rs }, NewCeiling(6, 2))
	now := time.Unix(0, 0)

	p.Decide("0.1", Message{ID: 1, Source: "pump", Subject: "opt_out"}, State{Notifiable: 1}, now)
	for i := 2; i <= 4; i++ {
		p.Decide("0.1", Message{ID: i, Source: "pump", Subject: "opt_out"},
			State{Notifiable: i}, now.Add(time.Duration(i)*time.Second))
	}

	// Drain the bucket right before the window expires.
	expired := now.Add(time.Hour + time.Second)
	for i := 5; i <= 6; i++ {
		p.Decide("0.1", Message{ID: i, Source: "other", Subject: "x", Urgent: true}, State{Notifiable: i}, expired)
	}

	capped := p.Decide("0.1", Message{ID: 7, Source: "pump", Subject: "opt_out"}, State{Notifiable: 7}, expired)
	if capped.Action != Suppress {
		t.Fatalf("Action = %v, want Suppress -- the ceiling must cap the rollup", capped.Action)
	}

	// Once tokens refill, the rollup must still report all 3 swallowed
	// messages; a capped attempt that reset the count would lose them forever.
	later := expired.Add(time.Minute)
	d := p.Decide("0.1", Message{ID: 8, Source: "pump", Subject: "opt_out"}, State{Notifiable: 8}, later)
	if d.Action != Rollup {
		t.Fatalf("Action = %v, want Rollup once the ceiling refills", d.Action)
	}
	if !strings.Contains(d.Text, "3×") {
		t.Fatalf("rollup must still report all 3 swallowed messages, got %q", d.Text)
	}
}

// The message that triggers an expiry rollup opens the next window: it is
// neither delivered on its own nor counted in the rollup it triggered.
func TestExpiryRollupAbsorbsItsTriggerMessage(t *testing.T) {
	rs := NewRuleSet()
	_ = rs.Add(Rule{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour})
	p := NewPolicy(func(addr.Address) *RuleSet { return rs }, NewCeiling(6, 6))
	now := time.Unix(0, 0)

	p.Decide("0.1", Message{ID: 1, Source: "pump", Subject: "opt_out"}, State{Notifiable: 1}, now)
	p.Decide("0.1", Message{ID: 2, Source: "pump", Subject: "opt_out"}, State{Notifiable: 2}, now.Add(time.Minute))
	p.Decide("0.1", Message{ID: 3, Source: "pump", Subject: "opt_out"}, State{Notifiable: 3}, now.Add(time.Minute))

	d := p.Decide("0.1", Message{ID: 4, Source: "pump", Subject: "opt_out"}, State{Notifiable: 4}, now.Add(2*time.Hour))
	if d.Action != Rollup {
		t.Fatalf("Action = %v, want Rollup", d.Action)
	}
	if !strings.Contains(d.Text, "2×") {
		t.Fatalf("rollup must count only the 2 swallowed messages, not its own trigger, got %q", d.Text)
	}
	if len(d.MarkRead) != 0 {
		t.Fatalf("MarkRead = %v, want empty -- a rollup delivers no bodies", d.MarkRead)
	}

	// The trigger opened the next window, so its follower is muted again.
	next := p.Decide("0.1", Message{ID: 5, Source: "pump", Subject: "opt_out"},
		State{Notifiable: 5}, now.Add(2*time.Hour+time.Minute))
	if next.Action != Suppress || !next.MarkMuted {
		t.Fatalf("Action = %v MarkMuted = %v, want the trigger to have reopened the window", next.Action, next.MarkMuted)
	}
}

// A message the ceiling capped must not buy 3s of silence it never paid for.
func TestCappedMessageDoesNotOpenCoalesceWindow(t *testing.T) {
	// 1 token, refilling at 1/s.
	p := NewPolicy(func(addr.Address) *RuleSet { return NewRuleSet() }, NewCeiling(60, 1))
	now := time.Unix(0, 0)

	// Urgent, so it bypasses coalescing entirely and only spends the token.
	p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s", Urgent: true}, State{Notifiable: 1}, now)

	capped := p.Decide("0.1", Message{ID: 2, Source: "0.2", Subject: "s"},
		State{Notifiable: 2}, now.Add(500*time.Millisecond))
	if capped.Action != Suppress {
		t.Fatalf("Action = %v, want Suppress -- the bucket is empty", capped.Action)
	}

	// Well inside what would have been the capped message's window, but the
	// bucket has refilled: this message must be written, not swallowed by a
	// window that a capped message had no right to open.
	d := p.Decide("0.1", Message{ID: 3, Source: "0.2", Subject: "s"},
		State{Notifiable: 3}, now.Add(1500*time.Millisecond))
	if d.Action == Suppress {
		t.Fatal("a capped message must not open a coalescing window that swallows its successors")
	}
}

// TestBacklogCostsOneNoticeRegardlessOfDepth pins the cost contract this whole
// phase exists to protect: a recipient's attention is spent per NOTICE, not
// per message, so a backlog that nobody has read yet must cost exactly one
// notice no matter how many messages pile into it. The recipient drains them
// all in the single inbox() call the standing notice already asked for.
//
// This is the regression gate on INV-2 being a STATE test rather than a growth
// test. Comparing counts (Notifiable > Announced -> announce) also passes a
// two-message check, which is why this sweeps depths: it fails on the second
// message and every one after, so the notice count tracks N instead of staying
// at 1 -- a turn spent per message, forever, for a slow trickle.
func TestBacklogCostsOneNoticeRegardlessOfDepth(t *testing.T) {
	big := strings.Repeat("x", InlineMaxBytes+1) // too large to inline, so nothing is consumed

	for _, depth := range []int{1, 2, 5, 50} {
		p := NewPolicy(func(addr.Address) *RuleSet { return nil }, NewCeiling(DefaultCeilingPerMinute, DefaultCeilingBurst))
		now := time.Unix(0, 0)

		notices, announced := 0, 0
		for i := 1; i <= depth; i++ {
			// Urgent so coalescing is bypassed entirely: this must hold on the
			// message-by-message path, not only because a batcher hid it.
			d := p.Decide("0.1",
				Message{ID: i, Source: "0.2", Subject: "s", Body: big, Urgent: true},
				State{Notifiable: i, Announced: announced},
				// Spread well past CoalesceWindow so no window is ever open,
				// and past the ceiling's refill so INV-1 is not what is doing
				// the suppressing -- otherwise this would pass even if INV-2
				// were removed outright.
				now.Add(time.Duration(i)*time.Minute))
			if d.Action == Suppress {
				continue
			}
			notices++
			announced = d.Announce
		}

		if notices != 1 {
			t.Fatalf("a backlog of %d unread messages cost %d notices, want exactly 1 -- "+
				"the recipient drains them all in one inbox() call, so anything above 1 "+
				"is a turn spent per message", depth, notices)
		}
	}
}

// TestReadingResetsTheAnnouncedBacklog: once the recipient has drained its
// inbox the caller reports Announced=0, and the next arrival earns a fresh
// notice. Without this the one-notice-per-backlog rule above would be a
// permanent gag rather than a per-batch one.
func TestReadingResetsTheAnnouncedBacklog(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return nil }, NewCeiling(DefaultCeilingPerMinute, DefaultCeilingBurst))
	now := time.Unix(0, 0)
	big := strings.Repeat("x", InlineMaxBytes+1)

	d := p.Decide("0.1", Message{ID: 1, Source: "0.2", Subject: "s", Body: big, Urgent: true}, State{Notifiable: 1, Announced: 0}, now)
	if d.Action != Notice {
		t.Fatalf("first message = %v, want Notice", d.Action)
	}
	if d.Announce != 1 {
		t.Fatalf("Announce = %d, want 1 (one message announced and unconsumed)", d.Announce)
	}
	if s := p.Decide("0.1", Message{ID: 2, Source: "0.2", Subject: "s", Body: big, Urgent: true}, State{Notifiable: 2, Announced: d.Announce}, now.Add(time.Minute)); s.Action != Suppress {
		t.Fatalf("second message = %v, want Suppress while a notice is outstanding", s.Action)
	}
	// The recipient reads: the caller's announced level goes to 0.
	if r := p.Decide("0.1", Message{ID: 3, Source: "0.2", Subject: "s", Body: big, Urgent: true}, State{Notifiable: 1, Announced: 0}, now.Add(2*time.Minute)); r.Action != Notice {
		t.Fatalf("after reading, a new message = %v, want Notice", r.Action)
	}
}

// TestRecoverGates pins the sweep half of the single decision point. It shares
// INV-2 and INV-1 with Decide, which is the entire reason it lives here: the
// 632fe95 flood came from a sweep that could re-emit a backlog on its own terms.
func TestRecoverGates(t *testing.T) {
	p := NewPolicy(func(addr.Address) *RuleSet { return nil }, NewCeiling(DefaultCeilingPerMinute, DefaultCeilingBurst))
	now := time.Unix(0, 0)

	if d := p.Recover("0.1", State{Notifiable: 0, Announced: 0}, true, now); d.Action != Suppress {
		t.Fatalf("empty backlog = %v, want Suppress (nothing to announce)", d.Action)
	}
	// Never announced: the anti-stall direction. A backlog nobody was told
	// about must always be announced, stale or not.
	d := p.Recover("0.1", State{Notifiable: 2, Announced: 0}, false, now)
	if d.Action != Notice || d.Announce != 2 {
		t.Fatalf("unannounced backlog = %v/%d, want Notice/2", d.Action, d.Announce)
	}
	// Already announced and the notice is fresh: silence. This is the flood
	// direction — without it every sweep re-emits the same backlog.
	if s := p.Recover("0.1", State{Notifiable: 2, Announced: 2}, false, now); s.Action != Suppress {
		t.Fatalf("fresh announced backlog = %v, want Suppress", s.Action)
	}
	// Stale overrides: a notice that never landed is retried.
	if r := p.Recover("0.1", State{Notifiable: 2, Announced: 2}, true, now); r.Action != Notice {
		t.Fatalf("stale announced backlog = %v, want Notice (the safety net)", r.Action)
	}
	// INV-1 still bounds it: a sweep cannot re-emit without limit even if
	// every notice keeps looking stale.
	capped := false
	for i := 0; i < 40; i++ {
		x := p.Recover("0.1", State{Notifiable: 2, Announced: 2}, true, now)
		if x.Action == Suppress && x.Capped {
			capped = true
			break
		}
	}
	if !capped {
		t.Fatal("the sweep must be subject to the INV-1 ceiling; it re-emitted without limit")
	}
}
