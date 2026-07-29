package notify

import (
	"errors"
	"fmt"
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

// TestReapExpiredRemovesOnlyExpired: the reap must take the expired rules and
// nothing else — live rules and TTL-less rules survive, in order.
func TestReapExpiredRemovesOnlyExpired(t *testing.T) {
	rs := NewRuleSet()
	add := func(r Rule) {
		t.Helper()
		if err := rs.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	add(Rule{ID: "forever", Source: "a", Created: tnow})                   // no TTL
	add(Rule{ID: "expired", Source: "b", TTL: time.Hour, Created: tnow})   // expires at +1h
	add(Rule{ID: "live", Source: "c", TTL: 10 * time.Hour, Created: tnow}) // still live at +2h
	add(Rule{ID: "stampless", Source: "d", TTL: time.Hour})                // zero Created: fails closed

	if n := rs.ReapExpired(tnow.Add(2 * time.Hour)); n != 2 {
		t.Fatalf("reaped %d rules, want 2 (expired + stampless)", n)
	}
	var got []string
	for _, r := range rs.List() {
		got = append(got, r.ID)
	}
	if len(got) != 2 || got[0] != "forever" || got[1] != "live" {
		t.Fatalf("survivors = %v, want [forever live] in insertion order", got)
	}
	if n := rs.ReapExpired(tnow.Add(2 * time.Hour)); n != 0 {
		t.Fatalf("second reap removed %d, want 0 (reaping is idempotent)", n)
	}
}

// TestReapIsInvisibleToMatch is the load-bearing test for the whole feature:
// the reap is a memory/quota cleanup and must never be a behaviour change. For
// every input, across a spread of clock readings, Match must return the exact
// same answer before and after a reap at the same instant.
func TestReapIsInvisibleToMatch(t *testing.T) {
	rules := []Rule{
		{ID: "forever", Source: "pump", Created: tnow},
		{ID: "short", Source: "pump", SubjectRe: "^deploy", TTL: time.Hour, Created: tnow},
		{ID: "long", SubjectRe: "^noise", TTL: 8 * time.Hour, Created: tnow},
		{ID: "body", BodyRe: "stack trace", TTL: 3 * time.Hour, Created: tnow},
		{ID: "stampless", Source: "ghost", TTL: time.Hour},
	}
	build := func() *RuleSet {
		rs := NewRuleSet()
		for _, r := range rules {
			if err := rs.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		return rs
	}

	inputs := []struct{ source, subject, body string }{
		{"pump", "deploy failed", ""},
		{"pump", "something else", ""},
		{"0.1", "noise from ci", ""},
		{"0.1", "quiet", "a stack trace here"},
		{"ghost", "anything", ""},
		{"unknown", "unmatched", "nothing"},
	}
	// Straddle every TTL boundary in the set.
	for _, d := range []time.Duration{0, 30 * time.Minute, 2 * time.Hour, 4 * time.Hour, 9 * time.Hour, 1000 * time.Hour} {
		now := tnow.Add(d)
		for _, in := range inputs {
			before := build()
			wantRule, wantOK := before.Match(in.source, in.subject, in.body, now)

			after := build()
			after.ReapExpired(now)
			gotRule, gotOK := after.Match(in.source, in.subject, in.body, now)

			if gotOK != wantOK {
				t.Fatalf("at +%v input %+v: matched=%v after reap, want %v", d, in, gotOK, wantOK)
			}
			if wantOK && gotRule.ID != wantRule.ID {
				t.Fatalf("at +%v input %+v: matched rule %q after reap, want %q", d, in, gotRule.ID, wantRule.ID)
			}
		}
	}
}

// TestReapFreesMaxRulesQuota: the point of reaping. A bubble that spent its
// whole quota on rules that have since expired must be able to add again.
func TestReapFreesMaxRulesQuota(t *testing.T) {
	rs := NewRuleSet()
	for i := 0; i < MaxRules; i++ {
		if err := rs.Add(Rule{ID: fmt.Sprintf("r%d", i), Source: "s", TTL: time.Hour, Created: tnow}); err != nil {
			t.Fatalf("filling the set: %v", err)
		}
	}
	if err := rs.Add(Rule{ID: "over", Source: "s"}); !errors.Is(err, ErrTooManyRules) {
		t.Fatalf("full set accepted a rule: %v", err)
	}
	if n := rs.ReapExpired(tnow.Add(2 * time.Hour)); n != MaxRules {
		t.Fatalf("reaped %d, want %d", n, MaxRules)
	}
	if err := rs.Add(Rule{ID: "fresh", Source: "s"}); err != nil {
		t.Fatalf("after reaping an all-expired set, Add still failed: %v", err)
	}
}

// TestReapExpiredRulesFiltersStoredSlice covers the persisted-form reaper,
// including the property that matters most for the registry: it never mutates
// or reorders the caller's slice, and returns it untouched when nothing expired.
func TestReapExpiredRulesFiltersStoredSlice(t *testing.T) {
	stored := []Rule{
		{ID: "a", Source: "x", Created: tnow},
		{ID: "b", Source: "y", TTL: time.Hour, Created: tnow},
		{ID: "c", Source: "z", TTL: 10 * time.Hour, Created: tnow},
	}
	orig := append([]Rule(nil), stored...)

	kept, n := ReapExpiredRules(stored, tnow.Add(2*time.Hour))
	if n != 1 || len(kept) != 2 || kept[0].ID != "a" || kept[1].ID != "c" {
		t.Fatalf("kept=%v n=%d, want [a c] and 1", kept, n)
	}
	for i := range orig {
		if stored[i] != orig[i] {
			t.Fatalf("input slice was mutated at %d: %v != %v", i, stored[i], orig[i])
		}
	}

	// An uncompilable rule must survive: rebuilding a RuleSet would silently
	// drop it, which is the permanent-deletion hazard MuteBy warns about.
	bad := []Rule{{ID: "bad", SubjectRe: "([unclosed", Created: tnow}}
	if _, err := CompileRule(bad[0]); err == nil {
		t.Fatal("test needs a pattern that genuinely fails to compile")
	}
	kept, n = ReapExpiredRules(bad, tnow.Add(100*time.Hour))
	if n != 0 || len(kept) != 1 {
		t.Fatalf("an uncompilable but unexpired rule was dropped: kept=%v n=%d", kept, n)
	}
}
