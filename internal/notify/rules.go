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
//
// EXPIRY IS ENFORCED HERE, at match time, and deliberately NOWHERE ELSE. A
// rule with TTL > 0 stops matching once now is more than TTL past Created; a
// TTL of 0 means "never expires". now is supplied by the caller because this
// package never reads the clock itself.
//
// The check must not move into CompileRule or RuleSet.Add: kernel.MuteBy
// rebuilds a RuleSet by re-adding every STORED rule and then persists
// rs.List(), so an Add that rejected expired rules would silently and
// permanently delete them from the registry. Reaping is a separate, explicit
// step — RuleSet.ReapExpired / ReapExpiredRules, which drop exactly the rules
// this check already refuses to honour and are therefore invisible here;
// matching is where the contract the mute()/mutes() tools advertise has to hold.
func (c *Compiled) Match(source, subject, body string, now time.Time) bool {
	if c.Expired(now) {
		return false
	}
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

// Expired reports whether r's TTL has elapsed by now. A zero TTL never
// expires; a zero Created with a non-zero TTL is treated as expired rather
// than immortal, so a rule that lost its creation stamp fails CLOSED (it stops
// muting) instead of silently deafening the bubble forever.
//
// This is the SINGLE definition of expiry in the process. Both the match path
// (Compiled.Expired -> Compiled.Match) and every reaper read it, so a reap can
// never remove a rule that Match would still have honoured — that equivalence
// is what makes reaping observationally invisible, and it is a property of
// there being one predicate, not of two predicates agreeing today.
func (r Rule) Expired(now time.Time) bool {
	if r.TTL <= 0 {
		return false
	}
	return now.Sub(r.Created) > r.TTL
}

// Expired reports whether c's rule has expired by now. See Rule.Expired.
func (c *Compiled) Expired(now time.Time) bool { return c.rule.Expired(now) }

// ReapExpired drops every rule that has expired by now, returning how many it
// removed. Remaining rules keep their relative order.
//
// This is the explicit reaping step Compiled.Match's comment defers to, and it
// is deliberately the ONLY thing it does. It removes exactly the rules that
// Match already refuses to honour, so for every input Match returns the same
// answer before and after a call — the reap reclaims memory and frees MaxRules
// quota, and can never change what gets muted. Anything more (renewing,
// re-sorting, re-compiling) would break that equivalence, which is why the
// expiry predicate lives in one place and this method just filters on it.
func (rs *RuleSet) ReapExpired(now time.Time) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	kept := rs.rules[:0]
	n := 0
	for _, c := range rs.rules {
		if c.Expired(now) {
			n++
			continue
		}
		kept = append(kept, c)
	}
	for i := len(kept); i < len(rs.rules); i++ {
		rs.rules[i] = nil // let the dropped Compiled (and its regexes) go
	}
	rs.rules = kept
	return n
}

// ReapExpiredRules filters a STORED rule slice, returning the survivors and how
// many were dropped. It works on Rule values rather than a RuleSet on purpose:
// the persisted form of a bubble's mute rules is []Rule, and rebuilding a
// RuleSet from it just to reap would run every rule back through Add — which
// silently skips anything that fails to compile, permanently deleting it on the
// next persist. That is exactly the hazard rules.go's Match comment and
// kernel.MuteBy warn about. Filtering the stored slice touches nothing but the
// expired entries.
//
// rules is never mutated; the caller gets a fresh slice when anything was
// dropped, and the original back (n == 0) when nothing was.
func ReapExpiredRules(rules []Rule, now time.Time) ([]Rule, int) {
	n := 0
	for _, r := range rules {
		if r.Expired(now) {
			n++
		}
	}
	if n == 0 {
		return rules, 0
	}
	kept := make([]Rule, 0, len(rules)-n)
	for _, r := range rules {
		if !r.Expired(now) {
			kept = append(kept, r)
		}
	}
	return kept, n
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
// source, subject, and body at now, and whether any rule matched at all.
// Expired rules are skipped but NOT removed -- see Compiled.Match for why
// reaping must stay out of the matching path.
func (rs *RuleSet) Match(source, subject, body string, now time.Time) (*Rule, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, c := range rs.rules {
		if c.Match(source, subject, body, now) {
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
