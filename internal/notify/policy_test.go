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
