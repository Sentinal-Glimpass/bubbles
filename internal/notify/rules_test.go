package notify

import (
	"strings"
	"testing"
	"time"
)

// tnow is the fixed clock these tests match against. notify never calls
// time.Now itself, so every Match takes its instant from the caller.
var tnow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

func TestMatchAllFieldsAreANDed(t *testing.T) {
	c, err := CompileRule(Rule{Source: "pump", SubjectRe: "^opt_out$", BodyRe: "noise"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "opt_out", "this is noise", tnow) {
		t.Fatal("all three match -> want true")
	}
	if c.Match("other", "opt_out", "this is noise", tnow) {
		t.Fatal("source differs -> want false")
	}
	if c.Match("pump", "reply", "this is noise", tnow) {
		t.Fatal("subject differs -> want false")
	}
	if c.Match("pump", "opt_out", "signal", tnow) {
		t.Fatal("body differs -> want false")
	}
}

func TestOmittedFieldsMatchAnything(t *testing.T) {
	c, err := CompileRule(Rule{Source: "pump"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "anything", "at all", tnow) {
		t.Fatal("source-only rule must match any subject/body")
	}
}

func TestEmptyPredicateRejected(t *testing.T) {
	if _, err := CompileRule(Rule{}); err != ErrEmptyPredicate {
		t.Fatalf("err = %v, want ErrEmptyPredicate", err)
	}
}

func TestBadRegexRejectedAtCompileTime(t *testing.T) {
	if _, err := CompileRule(Rule{SubjectRe: "([unclosed"}); err == nil {
		t.Fatal("invalid regex must be rejected at rule-creation time")
	}
}

func TestPatternLengthCapped(t *testing.T) {
	long := strings.Repeat("a", MaxPatternLen+1)
	if _, err := CompileRule(Rule{SubjectRe: long}); err != ErrPatternTooLong {
		t.Fatalf("err = %v, want ErrPatternTooLong", err)
	}
}

func TestBodyMatchBoundedTo4KB(t *testing.T) {
	c, err := CompileRule(Rule{BodyRe: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", MaxBodyMatchBytes) + "needle"
	if c.Match("", "", body, tnow) {
		t.Fatal("body beyond MaxBodyMatchBytes must not be scanned")
	}
}

func TestRuleSetCapsAt32(t *testing.T) {
	rs := NewRuleSet()
	for i := 0; i < MaxRules; i++ {
		if err := rs.Add(Rule{ID: string(rune('a' + i)), Source: "s"}); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := rs.Add(Rule{ID: "overflow", Source: "s"}); err != ErrTooManyRules {
		t.Fatalf("err = %v, want ErrTooManyRules", err)
	}
}

func TestRuleSetMatchReturnsFirstMatch(t *testing.T) {
	rs := NewRuleSet()
	_ = rs.Add(Rule{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour})
	got, ok := rs.Match("pump", "opt_out", "", tnow)
	if !ok || got.ID != "r1" {
		t.Fatalf("Match = %+v, %v; want r1, true", got, ok)
	}
	if _, ok := rs.Match("pump", "reply", "", tnow); ok {
		t.Fatal("non-matching subject must not match")
	}
}

// TestTTLIsEnforcedAtMatchTime pins the contract the mute()/mutes() tools
// advertise ("an optional ttl after which the rule expires"): expiry is a
// MATCHING property, checked against the caller's clock. Before this, TTL was
// stored, displayed, and never consulted -- a bubble that muted a source with
// ttl=1h was permanently deaf to it.
func TestTTLIsEnforcedAtMatchTime(t *testing.T) {
	created := tnow
	c, err := CompileRule(Rule{ID: "r", Source: "pump", TTL: time.Hour, Created: created})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "any", "", created.Add(59*time.Minute)) {
		t.Fatal("a rule inside its TTL must still match")
	}
	if c.Match("pump", "any", "", created.Add(61*time.Minute)) {
		t.Fatal("a rule past its TTL must not match -- the bubble would be permanently deaf to the source")
	}
}

// TestZeroTTLNeverExpires: ttl is optional, and omitting it means "until I
// unmute", not "immediately".
func TestZeroTTLNeverExpires(t *testing.T) {
	c, err := CompileRule(Rule{ID: "r", Source: "pump", Created: tnow})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "any", "", tnow.Add(100*24*time.Hour)) {
		t.Fatal("TTL == 0 must never expire")
	}
}

// TestRuleSetMatchSkipsExpiredRulesWithoutRemovingThem: expiry must not be a
// mutation. kernel.MuteBy rebuilds a RuleSet from stored rules and PERSISTS
// List(), so a matcher that reaped would delete rules from the registry.
func TestRuleSetMatchSkipsExpiredRulesWithoutRemovingThem(t *testing.T) {
	rs := NewRuleSet()
	if err := rs.Add(Rule{ID: "expired", Source: "pump", TTL: time.Hour, Created: tnow}); err != nil {
		t.Fatal(err)
	}
	later := tnow.Add(2 * time.Hour)
	if _, ok := rs.Match("pump", "x", "", later); ok {
		t.Fatal("an expired rule must not match")
	}
	if len(rs.List()) != 1 {
		t.Fatalf("an expired rule must stay in the set (reaping is a separate step), got %d", len(rs.List()))
	}
}
