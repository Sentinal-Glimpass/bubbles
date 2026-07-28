// Package notify holds the mute-rule predicate matcher a bubble uses to
// declare inbound traffic as noise, so the kernel stops waking it for that
// traffic. Predicates compile at rule-creation time (not at match time) so a
// bad pattern fails loudly to the calling bubble instead of silently
// swallowing every message at delivery.
package notify

import (
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Limits are deliberately small: a bubble that needs more than a handful of
// mute rules probably needs a different notification strategy, and a huge
// pattern or body scan window is a cost/DoS surface for no real benefit.
const (
	MaxRules          = 32
	MaxPatternLen     = 512
	MaxBodyMatchBytes = 4096
)

// Sentinel errors so a calling bubble can branch on cause rather than parse
// a string.
var (
	ErrTooManyRules   = errors.New("notify: rule set is full")
	ErrPatternTooLong = errors.New("notify: pattern exceeds MaxPatternLen")
	ErrEmptyPredicate = errors.New("notify: rule has no predicate (Source, SubjectRe, and BodyRe all empty)")
)

// Rule is a mute predicate: a message matches when Source (exact string,
// empty = any), SubjectRe, and BodyRe (regexes, empty = any) all match. AND
// semantics only — an OR rule is just two Rules in the same RuleSet, since
// RuleSet.Match already stops at the first hit.
type Rule struct {
	ID        string
	Source    string
	SubjectRe string
	BodyRe    string
	Window    time.Duration
	TTL       time.Duration
	Created   time.Time
}

// Compiled is a Rule with its regexes pre-parsed, so matching never pays a
// compile cost and never fails at match time.
type Compiled struct {
	rule    Rule
	subject *regexp.Regexp // nil means "match any subject"
	body    *regexp.Regexp // nil means "match any body"
}

// regexCache lets repeated identical patterns (e.g. reused across bubbles or
// re-added rules) skip re-compiling. It has its own mutex, independent of any
// RuleSet, since it is shared package-wide state.
var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

func compilePattern(pattern string) (*regexp.Regexp, error) {
	regexCacheMu.Lock()
	if re, ok := regexCache[pattern]; ok {
		regexCacheMu.Unlock()
		return re, nil
	}
	regexCacheMu.Unlock()

	// RE2 (Go's regexp) is linear-time with no backtracking, so an
	// adversarial pattern can't blow up match time — no timeout needed here.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	regexCacheMu.Lock()
	regexCache[pattern] = re
	regexCacheMu.Unlock()
	return re, nil
}

// CompileRule validates and compiles r. Order matters and is part of the
// contract: an all-empty predicate is rejected before pattern length is
// checked, which is checked before the regex is parsed — so the caller
// always learns the most fundamental problem first. Compile errors are
// returned unwrapped so their text (e.g. "error parsing regexp: ...") is
// visible to the bubble that submitted the bad pattern.
func CompileRule(r Rule) (*Compiled, error) {
	if r.Source == "" && r.SubjectRe == "" && r.BodyRe == "" {
		return nil, ErrEmptyPredicate
	}
	if len(r.SubjectRe) > MaxPatternLen || len(r.BodyRe) > MaxPatternLen {
		return nil, ErrPatternTooLong
	}

	var subject, body *regexp.Regexp
	var err error
	if r.SubjectRe != "" {
		subject, err = compilePattern(r.SubjectRe)
		if err != nil {
			return nil, err
		}
	}
	if r.BodyRe != "" {
		body, err = compilePattern(r.BodyRe)
		if err != nil {
			return nil, err
		}
	}

	return &Compiled{rule: r, subject: subject, body: body}, nil
}

// Match reports whether source, subject, and body all satisfy the rule's
// predicates (AND semantics; an empty/nil predicate matches anything). body
// is truncated to MaxBodyMatchBytes before scanning, so a rule can never be
// used to force an unbounded regex pass over an arbitrarily large message.
func (c *Compiled) Match(source, subject, body string) bool {
	if c.rule.Source != "" && c.rule.Source != source {
		return false
	}
	if c.subject != nil && !c.subject.MatchString(subject) {
		return false
	}
	if c.body != nil {
		if len(body) > MaxBodyMatchBytes {
			body = body[:MaxBodyMatchBytes]
		}
		if !c.body.MatchString(body) {
			return false
		}
	}
	return true
}

// RuleSet is an ordered collection of compiled rules for one bubble. Order is
// insertion order and is preserved across Add/Remove: Match returns the
// first rule that matches, and List must be stable so a bubble inspecting
// its own rules sees them in the order it added them.
type RuleSet struct {
	mu    sync.Mutex
	rules []*Compiled
}

// NewRuleSet returns an empty RuleSet.
func NewRuleSet() *RuleSet {
	return &RuleSet{}
}

// Add compiles and appends r, enforcing MaxRules so one bubble can't grow an
// unbounded predicate list that every inbound message must be scanned
// against.
func (rs *RuleSet) Add(r Rule) error {
	c, err := CompileRule(r)
	if err != nil {
		return err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.rules) >= MaxRules {
		return ErrTooManyRules
	}
	rs.rules = append(rs.rules, c)
	return nil
}

// Remove deletes the rule with the given id, reporting whether one was
// found. Remaining rules keep their relative order.
func (rs *RuleSet) Remove(id string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i, c := range rs.rules {
		if c.rule.ID == id {
			rs.rules = append(rs.rules[:i], rs.rules[i+1:]...)
			return true
		}
	}
	return false
}

// List returns the rules in insertion order, for display or persistence.
func (rs *RuleSet) List() []Rule {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]Rule, len(rs.rules))
	for i, c := range rs.rules {
		out[i] = c.rule
	}
	return out
}

// Match returns the first rule (in insertion order) whose predicate matches
// source, subject, and body, and whether any rule matched at all.
func (rs *RuleSet) Match(source, subject, body string) (*Rule, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, c := range rs.rules {
		if c.Match(source, subject, body) {
			r := c.rule
			return &r, true
		}
	}
	return nil, false
}

// String helps rules print usefully in logs/errors without dumping the
// compiled regex internals.
func (r Rule) String() string {
	return fmt.Sprintf("Rule{ID:%s Source:%q SubjectRe:%q BodyRe:%q}", r.ID, r.Source, r.SubjectRe, r.BodyRe)
}
