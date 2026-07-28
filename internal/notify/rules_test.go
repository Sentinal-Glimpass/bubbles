package notify

import (
	"strings"
	"testing"
	"time"
)

func TestMatchAllFieldsAreANDed(t *testing.T) {
	c, err := CompileRule(Rule{Source: "pump", SubjectRe: "^opt_out$", BodyRe: "noise"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "opt_out", "this is noise") {
		t.Fatal("all three match -> want true")
	}
	if c.Match("other", "opt_out", "this is noise") {
		t.Fatal("source differs -> want false")
	}
	if c.Match("pump", "reply", "this is noise") {
		t.Fatal("subject differs -> want false")
	}
	if c.Match("pump", "opt_out", "signal") {
		t.Fatal("body differs -> want false")
	}
}

func TestOmittedFieldsMatchAnything(t *testing.T) {
	c, err := CompileRule(Rule{Source: "pump"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Match("pump", "anything", "at all") {
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
	if c.Match("", "", body) {
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
	got, ok := rs.Match("pump", "opt_out", "")
	if !ok || got.ID != "r1" {
		t.Fatalf("Match = %+v, %v; want r1, true", got, ok)
	}
	if _, ok := rs.Match("pump", "reply", ""); ok {
		t.Fatal("non-matching subject must not match")
	}
}
