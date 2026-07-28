# Phase 0 + Phase 1: Cost Telemetry & Notification Economics — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop bubbles paying model turns for noise — give each bubble self-managed mute rules that suppress both the wake and the notice, inline small messages to skip the `inbox()` round-trip, and add the per-bubble counters that prove it worked.

**Architecture:** Extract notification policy out of `internal/kernel/kernel.go` into a pure `internal/notify` package with a single `Decide` entry point (no PTY, no kernel reference, no I/O), so the logic that caused the `632fe95` flood becomes table-testable. Add `internal/costmeter` first so every suppression is observable. The kernel keeps routing and performs exactly one action per `Decide` result.

**Tech Stack:** Go 1.x, stdlib only (`regexp` is RE2 — linear time, no backtracking). Existing test harness: `runner.NewFake()` / `FakeSession.Written()`.

**Spec:** `docs/superpowers/specs/2026-07-28-bubbles-cost-efficiency-design.md`

## Global Constraints

- **INV-1 (flood ceiling):** absolute cap of **6 notices per bubble per minute**, token-bucket, burst 6. Sits *below* policy; not disableable by any rule, capability, or config. Excess is counted in `NoticesCapped`, never delivered. This is the regression gate for commit `632fe95` (a 100–178× redelivery flood).
- **INV-2 (relaunch-correct dedup):** notification state keys to the *message backlog*, reconciled from the store — not to session lifetime, and never reset on relaunch. Ships with tests for BOTH failure directions (silent-stall and re-announce-flood).
- **No message is ever dropped.** Mute/coalescing/inlining affect *notification only*. `Store.UnreadCount()` stays truthful; every filed message stays readable via `inbox()`.
- **Every suppression path increments a costmeter counter.** Silent suppression is a defect.
- **Mute overrides `urgent`, and gates the WAKE, not just the notice.** Evaluation happens before the `urgent → EnsureAlive` page-in in `deliverMessage`.
- **Sanitisation:** any body that reaches a PTY MUST pass through `sanitizePTY` with newlines flattened. Bodies were previously never typed (see comment at `kernel.go:1289`); inlining changes that.
- **Do NOT flip the webhook `urgent` default** (`cmd/bubbles/webhook.go:95`, `:126`). Deferred by operator pending a pump audit. Leave both lines untouched.
- **File size:** no file created or substantially rewritten may exceed ~400 lines.
- **Extraction commits are behaviour-preserving and separate from behaviour-change commits.**
- `go build ./... && go test ./...` green at every task boundary.

**Rule limits (INV-1 aside):** ≤32 rules/bubble, pattern ≤512 bytes, `body_re` matched against first 4 KB of body only, compiled regexes cached by pattern string.

**Delivery defaults:** inline when sanitised+flattened body ≤280 bytes AND notifiable backlog ≤3 (all inlined together). Coalescing window 3 s, bypassed when `urgent=true`.

---

## File Structure

**Create:**
- `internal/costmeter/costmeter.go` — per-bubble counters, versioned like `inbox.Store` (~160 lines)
- `internal/costmeter/costmeter_test.go`
- `internal/notify/rules.go` — `Rule`, compile, match (predicate form D) (~150 lines)
- `internal/notify/rules_test.go`
- `internal/notify/ceiling.go` — INV-1 token bucket (~70 lines)
- `internal/notify/ceiling_test.go`
- `internal/notify/policy.go` — `Policy.Decide` (~220 lines)
- `internal/notify/policy_test.go`

**Modify:**
- `internal/inbox/inbox.go` — `Message.Muted`, `Store.NotifiableCount`, `Store.SetMuted`
- `internal/registry/registry.go` — `Bubble.MuteRules` + accessors
- `internal/kernel/kernel.go` — `deliverMessage` calls `notify.Decide`; delete `markNudge`/`recoverNudge` bodies in favour of policy state; single `shouldNotify`
- `cmd/bubbles/fleet.go` — persist `MuteRules` in `bubbleRec`
- `internal/mcpstdio/tools.go`, `internal/mcpstdio/server.go` — `mute`/`unmute`/`mutes` tools
- `cmd/bubbles/main.go:194` — `ipcBackend` methods for the three new tools
- `internal/tui/view.go:146` — `usagePanel` gains a counters block

---

## Task 1: costmeter package

**Files:**
- Create: `internal/costmeter/costmeter.go`
- Test: `internal/costmeter/costmeter_test.go`

**Interfaces:**
- Consumes: `internal/addr`
- Produces: `costmeter.New() *Meter`; `(*Meter).Add(a addr.Address, f Field, n int64)`; `(*Meter).Snapshot() map[addr.Address]Counters`; `(*Meter).Version() int64`; `Counters` struct with fields `NoticesWritten, NoticesSuppressed, NoticesCapped, DeliveriesInline, DeliveriesViaTool, TurnsTriggered, Evictions, Rewarms, ContextTokens int64`; `Field` enum constants `FNoticesWritten, FNoticesSuppressed, FNoticesCapped, FDeliveriesInline, FDeliveriesViaTool, FTurnsTriggered, FEvictions, FRewarms, FContextTokens`.

- [ ] **Step 1: Write the failing test**

```go
package costmeter

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestAddAndSnapshot(t *testing.T) {
	m := New()
	m.Add("0.1", FNoticesWritten, 1)
	m.Add("0.1", FNoticesWritten, 2)
	m.Add("0.1", FNoticesSuppressed, 5)
	m.Add("0.2", FDeliveriesInline, 1)

	snap := m.Snapshot()
	if got := snap[addr.Address("0.1")].NoticesWritten; got != 3 {
		t.Fatalf("0.1 NoticesWritten = %d, want 3", got)
	}
	if got := snap[addr.Address("0.1")].NoticesSuppressed; got != 5 {
		t.Fatalf("0.1 NoticesSuppressed = %d, want 5", got)
	}
	if got := snap[addr.Address("0.2")].DeliveriesInline; got != 1 {
		t.Fatalf("0.2 DeliveriesInline = %d, want 1", got)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	m := New()
	m.Add("0.1", FNoticesWritten, 1)
	snap := m.Snapshot()
	c := snap[addr.Address("0.1")]
	c.NoticesWritten = 99
	if m.Snapshot()[addr.Address("0.1")].NoticesWritten != 1 {
		t.Fatal("Snapshot must return a copy, not live state")
	}
}

func TestVersionBumpsOnChange(t *testing.T) {
	m := New()
	v0 := m.Version()
	m.Add("0.1", FNoticesWritten, 1)
	if m.Version() == v0 {
		t.Fatal("Version must bump on Add")
	}
}

func TestSetContextTokensReplacesNotAccumulates(t *testing.T) {
	m := New()
	m.Set("0.1", FContextTokens, 500_000)
	m.Set("0.1", FContextTokens, 620_000)
	if got := m.Snapshot()[addr.Address("0.1")].ContextTokens; got != 620_000 {
		t.Fatalf("ContextTokens = %d, want 620000 (Set replaces)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/costmeter/ -v`
Expected: FAIL — package does not compile, `New` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/costmeter/costmeter.go`. Requirements:
- `Field` is an `int` enum with the nine constants named above.
- `Counters` is a plain struct with the nine `int64` fields.
- `Meter` holds `mu sync.Mutex`, `ver int64`, `c map[addr.Address]*Counters`.
- `Add(a, f, n)` accumulates; `Set(a, f, n)` replaces (used for gauges like `ContextTokens`). Both bump `ver`.
- Use a single `field(*Counters, Field) *int64` helper so `Add`/`Set` share the switch — do not duplicate it.
- `Snapshot()` returns `map[addr.Address]Counters` by value (deep copy).
- `Version()` returns `ver` under the lock. This matches `inbox.Store.Version` so the existing `startSaver` pattern in `cmd/bubbles/app.go` can persist it later without a new mechanism.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/costmeter/ -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/costmeter/
git commit -m "feat(costmeter): per-bubble cost counters

Phase 0 of the cost/efficiency overhaul. Without per-bubble telemetry the
later phases are unfalsifiable -- a win and a regression look identical.
Versioned like inbox.Store so the existing saver pattern can persist it."
```

---

## Task 2: inbox Muted flag and NotifiableCount

**Files:**
- Modify: `internal/inbox/inbox.go`
- Test: `internal/inbox/inbox_test.go`

**Interfaces:**
- Produces: `Message.Muted bool` (JSON tag `muted,omitempty`); `(*Store).NotifiableCount(owner addr.Address) int`; `(*Store).SetMuted(id int)`.

**Critical:** `UnreadCount` must NOT change behaviour — it stays truthful and continues to back `inbox()`. `NotifiableCount` is a separate, additional query.

- [ ] **Step 1: Write the failing test**

```go
func TestNotifiableCountExcludesMuted(t *testing.T) {
	s := New()
	id1 := s.Append(Message{To: "0.1", Subject: "real"})
	id2 := s.Append(Message{To: "0.1", Subject: "noise"})
	s.SetMuted(id2)

	if got := s.UnreadCount("0.1"); got != 2 {
		t.Fatalf("UnreadCount = %d, want 2 (must stay truthful)", got)
	}
	if got := s.NotifiableCount("0.1"); got != 1 {
		t.Fatalf("NotifiableCount = %d, want 1", got)
	}
	_ = id1
}

func TestMutedMessagesAreStillReadable(t *testing.T) {
	s := New()
	id := s.Append(Message{To: "0.1", Subject: "noise", Body: "b"})
	s.SetMuted(id)
	got := s.Take("0.1")
	if len(got) != 1 || got[0].Subject != "noise" {
		t.Fatalf("muted message must still be returned by Take, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/inbox/ -run 'Muted|Notifiable' -v`
Expected: FAIL — `SetMuted` and `NotifiableCount` undefined.

- [ ] **Step 3: Write minimal implementation**

- Add `Muted bool \`json:"muted,omitempty"\`` to `Message`.
- `SetMuted(id int)` finds the message by ID, sets `Muted = true`, bumps `ver`.
- `NotifiableCount(owner)` mirrors `UnreadCount` (line 111) but skips `m.Muted`.
- Leave `UnreadCount`, `Take`, `Append`, `Snapshot`, `Load` otherwise untouched. `Muted` rides along in `Snapshot`/`Load` for free via the struct.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/inbox/ -v`
Expected: PASS — including all pre-existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/inbox/
git commit -m "feat(inbox): Muted flag and NotifiableCount

Muted affects NOTIFICATION only. UnreadCount stays truthful and muted
messages remain readable via Take -- no message is ever dropped."
```

---

## Task 3: notify mute rules (predicate form D)

**Files:**
- Create: `internal/notify/rules.go`, `internal/notify/rules_test.go`

**Interfaces:**
- Produces: `Rule{ID, Source string, SubjectRe, BodyRe string, Window, TTL time.Duration, Created time.Time}`; `CompileRule(r Rule) (*Compiled, error)`; `(*Compiled).Match(source, subject, body string) bool`; `RuleSet` with `Add(Rule) error`, `Remove(id string) bool`, `List() []Rule`, `Match(source, subject, body string) (*Rule, bool)`; errors `ErrTooManyRules`, `ErrPatternTooLong`, `ErrEmptyPredicate`.
- Constants: `MaxRules = 32`, `MaxPatternLen = 512`, `MaxBodyMatchBytes = 4096`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `internal/notify/rules.go`:
- Validate in `CompileRule` **before** compiling: all three predicate fields empty → `ErrEmptyPredicate`; either pattern longer than `MaxPatternLen` → `ErrPatternTooLong`; then `regexp.Compile` each non-empty pattern, returning the compile error directly so the calling bubble sees it.
- `Compiled` holds `rule Rule` plus `subject, body *regexp.Regexp` (nil when the pattern was empty).
- `Match`: empty `Source` matches any; non-empty requires exact equality. Nil `subject`/`body` regexes match any. For body, slice to `MaxBodyMatchBytes` first: `if len(body) > MaxBodyMatchBytes { body = body[:MaxBodyMatchBytes] }`.
- `RuleSet` holds `mu sync.Mutex` and an ordered `[]*Compiled` (order matters — `Match` returns the first hit, and `List` must be stable). `Add` enforces `MaxRules` and compiles (caching by pattern string in a package-level `map[string]*regexp.Regexp` guarded by its own mutex).
- Go's `regexp` is RE2: linear time, no backtracking. Do not add a match timeout — it is unreachable and would add complexity for nothing.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/ -v`
Expected: PASS (7 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/notify/
git commit -m "feat(notify): mute rules with source+subject+body predicates

Predicates compile at rule-creation time so a bad pattern fails loudly to
the calling bubble instead of silently at delivery. RE2 means no
backtracking DoS; bounds are on pattern length, body scan window, and
rule count."
```

---

## Task 4: notify flood ceiling (INV-1)

**Files:**
- Create: `internal/notify/ceiling.go`, `internal/notify/ceiling_test.go`

**Interfaces:**
- Produces: `NewCeiling(perMinute float64, burst int) *Ceiling`; `(*Ceiling).Allow(a addr.Address, now time.Time) bool`; constants `DefaultCeilingPerMinute = 6.0`, `DefaultCeilingBurst = 6`.

**This is the regression gate for `632fe95`.**

- [ ] **Step 1: Write the failing test**

```go
func TestCeilingCapsBurst(t *testing.T) {
	c := NewCeiling(DefaultCeilingPerMinute, DefaultCeilingBurst)
	now := time.Unix(0, 0)
	allowed := 0
	for i := 0; i < 178; i++ { // the observed 632fe95 flood size
		if c.Allow("0.1", now) {
			allowed++
		}
	}
	if allowed != DefaultCeilingBurst {
		t.Fatalf("allowed = %d, want %d -- INV-1 violated", allowed, DefaultCeilingBurst)
	}
}

func TestCeilingRefillsOverTime(t *testing.T) {
	c := NewCeiling(6, 6)
	now := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		c.Allow("0.1", now)
	}
	if c.Allow("0.1", now) {
		t.Fatal("bucket should be empty")
	}
	if !c.Allow("0.1", now.Add(10*time.Second)) { // 6/min = 1 per 10s
		t.Fatal("bucket should have refilled one token after 10s")
	}
}

func TestCeilingIsPerBubble(t *testing.T) {
	c := NewCeiling(6, 6)
	now := time.Unix(0, 0)
	for i := 0; i < 6; i++ {
		c.Allow("0.1", now)
	}
	if !c.Allow("0.2", now) {
		t.Fatal("0.2 must have its own bucket")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run Ceiling -v`
Expected: FAIL — `NewCeiling` undefined.

- [ ] **Step 3: Write minimal implementation**

Token bucket per address, mirroring the shape of the existing `rateLimiter` in `cmd/bubbles/webhook.go:271-300` (same algorithm, different scope — do not import it; that one is per-webhook-token and lives in `package main`). Fields: `mu sync.Mutex`, `rate float64` (tokens/sec = perMinute/60), `burst int`, `b map[addr.Address]*bucket{tokens float64; last time.Time}`. `Allow` refills by elapsed × rate, clamps to burst, spends one token if available.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/notify/
git commit -m "feat(notify): INV-1 flood ceiling, 6 notices/bubble/min

Hard backstop below the policy engine, not disableable. Regression gate
for 632fe95, which reverted prior nudge work after a single event was
re-emitted 100-178x fleet-wide. Test asserts a 178-message burst yields
exactly 6 notices."
```

---

## Task 5: notify Policy.Decide

**Files:**
- Create: `internal/notify/policy.go`, `internal/notify/policy_test.go`

**Interfaces:**
- Consumes: `Rule`/`RuleSet` (Task 3), `Ceiling` (Task 4).
- Produces:

```go
type Action int
const (
	Suppress Action = iota // file silently: no wake, no notice
	Notice                 // write the "you have mail" notice
	Inline                 // write the notice WITH bodies inlined; mark those read
	Rollup                 // write an "N× subject since T" summary
)

type Message struct {
	ID      int
	Source  string // sender label: bubble name, or webhook source
	Subject string
	Body    string
	Urgent  bool
}

type State struct {
	Notifiable  int  // Store.NotifiableCount(to)
	Announced   int  // backlog size already announced (INV-2: reconciled from store, not session)
	Hot         bool // recipient has a live session
	AlwaysOn    bool
}

type Decision struct {
	Action    Action
	Text      string // rendered, PTY-safe, single line; empty unless Notice/Inline/Rollup
	MarkRead  []int  // message ids consumed by an Inline delivery
	MarkMuted bool   // caller must call Store.SetMuted for this message
	Wake      bool   // caller may page in a cold bubble (false => never wake)
	Rule      string // id of the matching mute rule, "" if none
}

func (p *Policy) Decide(to addr.Address, msg Message, st State, now time.Time) Decision
```

- `NewPolicy(rules func(addr.Address) *RuleSet, ceiling *Ceiling) *Policy`
- `(*Policy).Pending(to addr.Address, now time.Time) (Decision, bool)` — drains a coalescing window that has expired.
- `(*Policy).Clear(to addr.Address)` — called when the bubble reads its inbox.
- Constants: `InlineMaxBytes = 280`, `InlineMaxBacklog = 3`, `CoalesceWindow = 3 * time.Second`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run Policy -v`
Expected: FAIL — `NewPolicy` undefined.

- [ ] **Step 3: Write minimal implementation**

`internal/notify/policy.go`. Decision order — this order is load-bearing:

1. **Mute check first** (before anything urgency-related). If a rule matches: if inside its window → `{Action: Suppress, MarkMuted: true, Wake: false, Rule: id}`. If the window expired → reset the window, return `Rollup` with text `fmt.Sprintf("📬 %d× %q from %s since %s — call inbox() to read.", n, subject, source, since.Format("15:04"))`.
2. **INV-2 dedup:** if `st.Notifiable <= st.Announced` → `Suppress`. Compare against the store-derived `Announced`, never against session state and never against a policy-local counter — a local high-water mark that can drift above the store's truth reintroduces the *silent-stall* failure direction of `632fe95`.
3. **Coalescing:** non-urgent and window open → buffer and `Suppress`; `Pending` drains it later. Urgent bypasses.
4. **INV-1 ceiling:** `if !ceiling.Allow(to, now) → {Action: Suppress}` and the caller records `FNoticesCapped`.
5. **Inline vs Notice:** sanitise + flatten the body; if `len(clean) <= InlineMaxBytes && st.Notifiable <= InlineMaxBacklog` → `Inline` with `MarkRead`; else `Notice`.

> **Order correction (2026-07-28, approved mid-execution).** This list originally
> placed the ceiling at step 3, *before* coalescing. That was wrong: a message
> spent a ceiling token and was then suppressed by coalescing, so tokens burned
> on messages that produced no notice — contradicting INV-1's own definition as a
> cap on notices *written*, and letting a burst drain the bucket so genuine later
> notices were capped. It also made `TestCeilingOverridesPolicy` vacuous (178
> non-urgent messages coalesced down to `written == 1`, trivially satisfying a
> `<= 6` assertion without ever exercising the ceiling). The ceiling is now the
> last gate before any non-Suppress action, and the test uses `Urgent: true`
> messages with an exact `== DefaultCeilingBurst` assertion. The rollup path
> keeps its own ceiling check — a rollup is a real write.

Port `sanitizePTY` from `internal/kernel/kernel.go:1294` into this package as an unexported `sanitize` (it must not import kernel — that would be an import cycle). Flatten `\n`/`\r` to a single space *before* sanitising. **Leave the kernel copy in place for now**; Task 7 switches the kernel to the notify one and deletes the kernel copy, keeping the move separate from the behaviour change.

Keep `policy.go` under 400 lines; if it grows past that, split the rollup/text rendering into `internal/notify/render.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/notify/ -v`
Expected: PASS (all Task 3, 4, 5 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/notify/
git commit -m "feat(notify): pure Decide policy engine

Suppress/Notice/Inline/Rollup decided with no PTY, no kernel, no I/O, so
the logic that caused the 632fe95 flood is finally table-testable. Order
is load-bearing: mute -> INV-2 dedup -> INV-1 ceiling -> coalesce ->
inline. Mute is evaluated first so it can veto the wake."
```

---

## Task 6: persist mute rules on the registry

**Files:**
- Modify: `internal/registry/registry.go`, `cmd/bubbles/fleet.go`
- Test: `internal/registry/registry_test.go`, `cmd/bubbles/fleet_test.go`

**Interfaces:**
- Consumes: `notify.Rule` (Task 3).
- Produces: `Bubble.MuteRules []notify.Rule`; `(*Registry).SetMuteRules(a addr.Address, rules []notify.Rule)`; `(*Registry).MuteRules(a addr.Address) []notify.Rule`. Both bump `version` so the existing saver persists.

- [ ] **Step 1: Write the failing test**

```go
// internal/registry/registry_test.go
func TestMuteRulesRoundTrip(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "w", "/tmp")
	r.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", Window: time.Hour}})
	got := r.MuteRules(a)
	if len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("MuteRules = %+v, want one rule r1", got)
	}
}

func TestSetMuteRulesBumpsVersion(t *testing.T) {
	r := New()
	a := r.Add(addr.Root, "w", "/tmp")
	v0 := r.Version()
	r.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump"}})
	if r.Version() == v0 {
		t.Fatal("SetMuteRules must bump version so fleet.json is re-saved")
	}
}
```

```go
// cmd/bubbles/fleet_test.go — mirror the existing save/load round-trip test style
func TestFleetPersistsMuteRules(t *testing.T) {
	dir := t.TempDir()
	k := kernel.New(runner.NewFake())
	a, _ := k.Spawn(addr.Root, "w", "/tmp", runner.SpawnOpts{Persona: "w"})
	k.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour}})
	if err := saveFleet(dir, k); err != nil {
		t.Fatal(err)
	}
	k2 := kernel.New(runner.NewFake())
	restoreFleet(dir, k2)
	got := k2.Reg.MuteRules(a)
	if len(got) != 1 || got[0].SubjectRe != "^opt_out$" {
		t.Fatalf("restored rules = %+v", got)
	}
}
```

Adjust `r.Add(...)` and `saveFleet`/`restoreFleet` call signatures to match the existing ones in those files — read them first and mirror the neighbouring tests exactly.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/ ./cmd/bubbles/ -run Mute -v`
Expected: FAIL — `SetMuteRules` undefined.

- [ ] **Step 3: Write minimal implementation**

- `Bubble.MuteRules []notify.Rule` — placed after `AlwaysOn`, documented in the same comment style as its neighbours.
- `SetMuteRules`/`MuteRules` under `Registry.mu`, both bumping `version`. `MuteRules` returns a copy.
- `bubbleRec` in `cmd/bubbles/fleet.go` gains `MuteRules []notify.Rule \`json:"muteRules,omitempty"\``. Wire it in the save path (line ~179) and the restore path (line ~244), exactly alongside `AlwaysOn`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/registry/ ./cmd/bubbles/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/registry/ cmd/bubbles/fleet.go cmd/bubbles/fleet_test.go
git commit -m "feat(registry): persist per-bubble mute rules in fleet.json

Rules live alongside AlwaysOn/Disabled and survive restart, so a bubble
that has muted a noisy pump stays muted."
```

---

## Task 7: wire notify into the kernel delivery path

**Files:**
- Modify: `internal/kernel/kernel.go` (`deliverMessage` ~493-560; `markNudge`/`recoverNudge`/`clearNudge` ~115-150; `sanitizePTY` ~1294)
- Test: `internal/kernel/kernel_test.go`

**Interfaces:**
- Consumes: `notify.Policy` (Task 5), `costmeter.Meter` (Task 1), `Registry.MuteRules` (Task 6), `Store.NotifiableCount`/`SetMuted` (Task 2).
- Produces: `Kernel.Notify *notify.Policy`, `Kernel.Cost *costmeter.Meter` (both set in `New`).

**This is the highest-risk task. It changes live delivery behaviour.**

- [ ] **Step 1: Write the failing test**

```go
func TestMutedUrgentMessageDoesNotWakeColdBubble(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.Reg.SetMuteRules(a, []notify.Rule{{ID: "r1", Source: "pump", SubjectRe: "^opt_out$", Window: time.Hour}})

	// Open the window with one delivery, then let it go cold.
	k.WebhookDeliver(a, "pump", "opt_out", "e1", true)
	_ = k.runner.Kill(a)
	k.smu.Lock()
	delete(k.sessions, a)
	k.smu.Unlock()

	// Second event inside the window must not page the bubble back in.
	k.WebhookDeliver(a, "pump", "opt_out", "e2", true)
	if k.IsHot(a) {
		t.Fatal("muted urgent webhook must NOT wake a cold bubble -- that is the rewarm cost")
	}
	if k.Store.UnreadCount(a) != 2 {
		t.Fatalf("both messages must still be filed, got %d", k.Store.UnreadCount(a))
	}
}

func TestSmallMessageIsInlinedAndNeedsNoInboxCall(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	k.Send(addr.Root, a, "status", "build green", 0, true)

	out := fr.Session(a).Written()
	if !strings.Contains(out, "build green") {
		t.Fatalf("short body must be inlined into the notice, got %q", out)
	}
	if strings.Contains(out, "call the inbox() tool") {
		t.Fatal("an inlined delivery must not also tell the bubble to call inbox()")
	}
}

func TestNoticeCountNeverExceedsCeiling(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)
	for i := 0; i < 178; i++ {
		k.Send(addr.Root, a, "s", "b", 0, true)
	}
	got := strings.Count(fr.Session(a).Written(), "📬")
	if got > notify.DefaultCeilingBurst {
		t.Fatalf("notices = %d, exceeds INV-1 ceiling %d", got, notify.DefaultCeilingBurst)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/kernel/ -run 'Muted|Inlined|Ceiling' -v`
Expected: FAIL — `k.Reg.SetMuteRules` compiles but the mute is ignored; the bubble wakes.

- [ ] **Step 3: Write minimal implementation**

In `New`, construct `Notify` with a rules lookup closure over the registry, and `Cost`.

Rewrite the tail of `deliverMessage` (from `unread := k.Store.UnreadCount(to)` onward). The new shape:

```go
st := notify.State{
    Notifiable: k.Store.NotifiableCount(to),
    Announced:  k.announced(to),
    Hot:        k.IsHot(to),
    AlwaysOn:   k.isAlwaysOn(to),
}
d := k.Notify.Decide(to, notify.Message{
    ID: id, Source: sourceLabel, Subject: subject, Body: body, Urgent: urgent,
}, st, time.Now())

if d.MarkMuted {
    k.Store.SetMuted(id)
    k.Cost.Add(to, costmeter.FNoticesSuppressed, 1)
}
if d.Action == notify.Suppress {
    return id   // filed, never notified, NEVER woken
}
```

Only after this point may the existing focus/typing hold, the `urgent → EnsureAlive` page-in, and the PTY write run — and the page-in is now additionally gated on `d.Wake`. Preserve verbatim: the `isFocused(to) && typingActive()` hold, the `InputReady()` check with its `deliverWhenReady` fallback, and the `b.SessionID == ""` first-launch case.

On write: `FNoticesWritten`, plus `FDeliveriesInline` and `Store` read-marking for each `d.MarkRead` id, or `FDeliveriesViaTool` for a plain `Notice`.

`announced(to)` replaces the `notified` map as the INV-2 source — it returns the backlog size last announced, and is cleared by `Inbox()` (existing `clearNudge` call site). Delete `markNudge`/`recoverNudge` and route `RecoverUnread` through `Decide` too (Task 8).

Delete `sanitizePTY` from `kernel.go` and use `notify`'s. **Commit that deletion separately** from the behaviour change, per the non-regression discipline.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... `
Expected: PASS — including every pre-existing kernel test. If a pre-existing test now fails, STOP and report it; do not weaken the test.

- [ ] **Step 5: Commit**

```bash
git add internal/kernel/
git commit -m "feat(kernel): route delivery through notify.Decide

Mute is evaluated BEFORE the urgent page-in, so a muted webhook no longer
wakes a cold bubble and pays a prompt-cache rewarm -- the dominant cost.
Messages are always filed; only notification changes."
```

---

## Task 8: single shouldNotify — fix the double-notice race

**Files:**
- Modify: `internal/kernel/kernel.go` (`RecoverUnread` ~590-645)
- Test: `internal/kernel/kernel_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSendAndRecoverDoNotDoubleNotify(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	k.EnsureAlive(a)

	k.Send(addr.Root, a, "real", strings.Repeat("x", notify.InlineMaxBytes+1), 0, true)
	k.RecoverUnread(true) // the 45s sweep, racing the send path

	if got := strings.Count(fr.Session(a).Written(), "📬"); got != 1 {
		t.Fatalf("notices = %d, want 1 -- send path and recovery sweep double-notified", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/kernel/ -run DoubleNotify -v`
Expected: FAIL — two notices written.

- [ ] **Step 3: Write minimal implementation**

Replace `RecoverUnread`'s bespoke `recoverNudge` + `formatDrain` with the same `Decide` call used by `deliverMessage`, extracted into one unexported helper used by both:

```go
func (k *Kernel) shouldNotify(to addr.Address, msg notify.Message, now time.Time) notify.Decision
```

The `NotifiableCount` read, the `Decide` call, and the `announced` update happen under a single lock inside the policy, so two goroutines cannot both decide to notify the same backlog. `RecoverUnread` keeps its existing structure otherwise — the `hotOnly` split, the 4-worker semaphore, the focused-bubble skip, and the `EnsureAlive` page-in for the cold sweep.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/kernel/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/kernel/
git commit -m "fix(kernel): single notify decision point kills the double-notice

The count read and the notify decision now happen under one lock, so the
send path and the 45s recovery sweep can no longer both announce the same
backlog -- observed live as two notices for one webhook event."
```

---

## Task 9: mute/unmute/mutes MCP tools

**Files:**
- Modify: `internal/mcpstdio/tools.go`, `internal/mcpstdio/server.go`, `cmd/bubbles/main.go:194`, `internal/kernel/kernel.go`
- Test: `internal/mcpstdio/mcp_test.go` (mirror existing tool tests), `internal/kernel/kernel_test.go`

**Interfaces:**
- Produces: `Backend.Mute(by, source, subjectRe, bodyRe, window, ttl string) (string, error)`, `Backend.Unmute(by, id string) error`, `Backend.Mutes(by string) []string`; kernel-side `(*Kernel).MuteBy`, `(*Kernel).UnmuteBy`, `(*Kernel).MutesFor`.

- [ ] **Step 1: Write the failing test**

```go
func TestMuteByRejectsBadRegexWithAUsefulError(t *testing.T) {
	k := New(runner.NewFake())
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	_, err := k.MuteBy(a, "pump", "([unclosed", "", "1h", "")
	if err == nil {
		t.Fatal("a bad regex must be rejected at rule-creation time")
	}
	if !strings.Contains(err.Error(), "error parsing regexp") {
		t.Fatalf("error must explain the regex failure, got %v", err)
	}
	if len(k.Reg.MuteRules(a)) != 0 {
		t.Fatal("a rejected rule must never be stored")
	}
}

func TestMuteByStoresAndUnmuteRemoves(t *testing.T) {
	k := New(runner.NewFake())
	a, _ := k.Spawn(addr.Root, "w", "/tmp/w", runner.SpawnOpts{Persona: "w"})
	id, err := k.MuteBy(a, "pump", "^opt_out$", "", "1h", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Reg.MuteRules(a)) != 1 {
		t.Fatal("rule must be stored")
	}
	if err := k.UnmuteBy(a, id); err != nil {
		t.Fatal(err)
	}
	if len(k.Reg.MuteRules(a)) != 0 {
		t.Fatal("rule must be removed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/kernel/ -run Mute -v`
Expected: FAIL — `MuteBy` undefined.

- [ ] **Step 3: Write minimal implementation**

- Kernel: `MuteBy` parses `window`/`ttl` with `time.ParseDuration`, mints an id (reuse the short-id style already used for schedules), calls `notify.CompileRule` to validate, and only on success appends via `Reg.SetMuteRules`. A bubble may only mute **its own** inbox — no target parameter, no cross-bubble muting.
- `internal/mcpstdio/tools.go`: three `Tool` entries appended to `ts` unconditionally (every bubble may manage its own noise). Descriptions must state that muting suppresses the *notification*, not the message, and that messages remain readable via `inbox()`.
- `internal/mcpstdio/server.go`: three `case` arms alongside the existing ones (~line 113-290), following the same argument-extraction and error-return shape as `case "schedule"`.
- `cmd/bubbles/main.go`: three `ipcBackend` methods relaying over IPC, mirroring the existing `Schedule`/`Unschedule`/`Schedules` trio exactly.
- Add the three tools to `citizenPrompt` in `cmd/bubbles/citizen.go`, in the Conventions section, with one line each.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... `
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcpstdio/ internal/kernel/ cmd/bubbles/
git commit -m "feat(mcp): mute/unmute/mutes tools

A bubble that recognises noise can now act on it instead of paying a turn
per event forever. Rules are self-scoped -- a bubble may only mute its own
inbox. Bad regexes fail at creation with the parse error."
```

---

## Task 10: fleet-health block in the top-right panel

**Files:**
- Modify: `internal/tui/view.go:146` (`usagePanel`), `internal/tui/model.go`
- Test: `internal/tui/view_test.go`

**Interfaces:**
- Consumes: `costmeter.Counters` (Task 1).
- Produces: `tui.FleetHealth{Hot, Total, Suppressed, Capped, Inlined, Backlog int}`; `fleetHealthRows(FleetHealth) []string`.

Per spec §8.1 this is a Phase 4 deliverable in full; this task lands the subset the costmeter can already feed. Stuck/crash-loop/failing-check metrics arrive in Phase 4 and must be **omitted, not rendered as zero**, until then.

- [ ] **Step 1: Write the failing test**

```go
func TestFleetHealthRowsOmitUnavailableMetrics(t *testing.T) {
	rows := fleetHealthRows(FleetHealth{Hot: 2, Total: 5, Suppressed: 40, Capped: 0, Inlined: 12, Backlog: 3})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "40") {
		t.Fatal("suppressed count must be shown")
	}
	if strings.Contains(joined, "stuck") {
		t.Fatal("Phase 4 metrics must be omitted, not rendered as zero")
	}
}

func TestUsagePanelIncludesFleetHealth(t *testing.T) {
	m := Model{}
	m.health = FleetHealth{Hot: 1, Total: 2, Suppressed: 7}
	joined := strings.Join(usagePanel(m), "\n")
	if !strings.Contains(joined, "FLEET") {
		t.Fatal("usagePanel must include the fleet health block")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'FleetHealth|UsagePanel' -v`
Expected: FAIL — `fleetHealthRows` undefined.

- [ ] **Step 3: Write minimal implementation**

- `Model` gains `health FleetHealth`, fed by a `FleetHealthMsg` pushed from the existing resource-sampling loop in `cmd/bubbles/app.go` (the 2 s ticker at line ~241) — no new poller.
- `fleetHealthRows` renders with the existing `panelHead`/`panelStyle`, headed `FLEET · n/N hot`, then a counters line `mute N · cap N · inline N` and a backlog line when non-zero. Colour via the existing `sevStyle`: green when `Capped == 0` and backlog is small, amber on backlog, red when `Capped > 0` (a capped notice means INV-1 is actively firing, which is worth seeing).
- Append `fleetHealthRows(u.health)...` to `usagePanel` after the `RESOURCES` block, before the per-bubble `Top` rows.
- Keep `fleetHealthRows` a pure function over the struct — no `Model` access, testable without a terminal.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tui/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui/ cmd/bubbles/app.go
git commit -m "feat(tui): fleet health block in the top-right usage panel

Cost and health read together. Fed by the costmeter via the existing 2s
resource sampler -- no new poller. Phase 4 metrics are omitted rather
than shown as zero until they exist."
```

---

## Task 11: end-to-end verification against the live leak

**Files:**
- Test: `internal/kernel/kernel_test.go`

- [ ] **Step 1: Write the failing test**

```go
// Reproduces the observed mechmagnet-event-pump leak end to end.
func TestMechmagnetNoiseCostsOneNoticePerWindow(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	a, _ := k.Spawn(addr.Root, "qa", "/tmp/qa", runner.SpawnOpts{Persona: "qa"})
	k.EnsureAlive(a)
	if _, err := k.MuteBy(a, "mechmagnet-event-pump", "^opt_out$", "", "1h", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		k.WebhookDeliver(a, "mechmagnet-event-pump", "opt_out", "event", true)
	}
	notices := strings.Count(fr.Session(a).Written(), "📬")
	if notices > 1 {
		t.Fatalf("notices = %d, want 1 for 200 muted events in one window", notices)
	}
	if k.Store.UnreadCount(a) != 200 {
		t.Fatalf("all 200 events must still be filed, got %d", k.Store.UnreadCount(a))
	}
	if k.Cost.Snapshot()[a].NoticesSuppressed != 199 {
		t.Fatalf("suppressions must be counted, got %d", k.Cost.Snapshot()[a].NoticesSuppressed)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/kernel/ -run Mechmagnet -v`
Expected: PASS if Tasks 1–9 are correct. If it fails, the defect is in Task 7's ordering — mute must be evaluated before the wake.

- [ ] **Step 3: Full suite + build**

```bash
go build ./... && go test ./...
```
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/kernel/kernel_test.go
git commit -m "test(kernel): end-to-end mechmagnet noise regression

200 muted webhook events -> 1 notice, 200 messages still filed, 199
suppressions counted. This is the leak that motivated the phase."
```

---

## Self-Review

**Spec coverage:**
- §5.1 mute rules → Tasks 3, 6, 9
- §5.2 urgent default → deliberately NOT implemented (Global Constraints); its consequence (mute gates the wake) → Task 7
- §5.3 inlining + sanitisation → Tasks 5, 7
- §5.4 coalescing → Task 5
- §5.5 double-notice → Task 8
- §4 costmeter → Task 1
- §8.1 panel → Task 10 (Phase-0-fed subset; remainder deferred to Phase 4, stated in-task)
- §3.2 `internal/notify` extraction → Tasks 3–5, 7
- INV-1 → Task 4 + Task 5 + Task 7 tests
- INV-2 both directions → Task 5 tests
- §9.1 non-regression → Global Constraints + Task 7 Step 4 stop-condition

**Not covered here (correctly — later plans):** §6 Phase 2, §7 Phase 3, §8 Phase 4, `internal/sessions` and `internal/supervisor` extractions.

**Known follow-ups:** mute-rule TTL expiry cleanup is a Phase 4 health check (spec §8); rules created with a TTL here are stored but not yet reaped.
