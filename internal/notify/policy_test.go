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
	for i := 1; i <= 178; i++ {
		d := p.Decide("0.1", Message{ID: i, Source: "0.2", Subject: "s"}, State{Notifiable: i}, now)
		if d.Action != Suppress {
			written++
		}
	}
	if written > DefaultCeilingBurst {
		t.Fatalf("written = %d, exceeds INV-1 ceiling %d", written, DefaultCeilingBurst)
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
