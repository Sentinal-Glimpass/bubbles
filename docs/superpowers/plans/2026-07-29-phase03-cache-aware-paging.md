# Phase 3: Cache-Aware Paging — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop paying token cost to save RAM the fleet was not short of. Make eviction aware that paging a bubble out throws away its prompt cache, and that rewarming it costs real money proportional to its context size.

**Architecture:** Extract the eviction *decision* into a pure, table-tested `internal/paging` policy (no kernel, no sessions, no I/O), then have the kernel apply it. Follows the Phase 1 shape: pure policy engine + thin kernel caller. Finally extract the session table itself into `internal/sessions`, behaviour-preserving, in commits separate from any behaviour change.

**Tech Stack:** Go, stdlib. Builds on Phase 0+1 (`cef78de`) and Phase 2 (`1e0e780`).

**Spec:** `docs/superpowers/specs/2026-07-28-bubbles-cost-efficiency-design.md` §7

---

## The problem, concretely

`cmd/bubbles/app.go:113-114` configures `MemBudget = 45 GB` and `IdleTimeout = 30 * time.Minute`.

- `EnforceBudget` (`internal/kernel/kernel.go:279`) sorts live sessions by `lastUsed` — a logical-clock LRU — and kills the coldest until resident memory fits the budget. It considers **resident bytes only**.
- `EvictIdle` (`kernel.go:394`) kills any session with no output for `IdleTimeout`.

Neither knows that killing a session **throws away its prompt cache**. When that bubble is next used it re-pays **full uncached input** for its entire context. A 600k-token bubble evicted at minute 30 and woken at minute 32 costs far more in one rewarm than the RAM was ever worth — and at 45 GB the budget is rarely the binding constraint anyway, so most evictions are `EvictIdle`'s doing, not memory pressure.

**The sharpest instance:** if the prompt-cache TTL is an hour, a 30-minute `IdleTimeout` evicts sessions whose caches are still warm. That is strictly wasteful — it converts a free wake into a paid one, with no memory benefit demanded by anything.

Phase 2 already populates `FContextTokens` per bubble (the compaction pump records it every sweep), and `FEvictions`/`FRewarms` already exist in `internal/costmeter`. Phase 3 is the consumer.

## Global Constraints

- **Behaviour-preserving extractions land in commits SEPARATE from behaviour changes** (spec §9.1.2). Task 5's move must not change what anything decides.
- **`internal/paging` is pure:** no `time.Now()` (all time caller-supplied), no kernel import, no I/O, no session interface. It takes facts and returns decisions.
- **Always-on receivers are never evicted**, by budget or idleness. Pre-existing law in both functions — do not weaken it.
- **Root is never evicted.**
- **A bubble under genuine memory pressure must still be evictable.** Cost-awareness reorders *who* is evicted; it must never let the fleet exceed `MemBudget`. If every remaining candidate is expensive, the most expensive still gets evicted rather than blowing the budget — that is the one place token cost must yield.
- Every eviction increments `FEvictions`; every rewarm (a resume that re-pays uncached input) increments `FRewarms`.
- Preserve lock order `notifyMu → Policy.mu → registry.mu`; never hold a lock across a PTY write or a `MemBytes()` probe (the existing code deliberately measures outside the lock — keep that).
- "No existing test is deleted or weakened to make new code pass." If a pre-existing test fails and you believe it encodes superseded behaviour, STOP and report the specific assertion.
- File ceiling ~400 lines.
- `go build ./... && go vet ./... && go test ./...` green at every task boundary, plus `-race` on touched packages.

---

## Task 1: `internal/paging` — pure eviction policy

**Files:** Create `internal/paging/paging.go`, `internal/paging/paging_test.go`.

**Interfaces:**
- Produces:
```go
type Candidate struct {
    Addr         addr.Address
    MemBytes     uint64
    ContextTokens int64     // 0 = unknown
    IdleFor      time.Duration
    AlwaysOn     bool
}
type Config struct {
    MemBudget int64
    CacheTTL  time.Duration // prompt-cache lifetime; below this an eviction throws away a live cache
}
// Victims returns, in eviction order, who to page out to fit the budget.
func Victims(c Config, live []Candidate, totalMem uint64) []addr.Address
// IdleVictims returns who is idle enough that eviction is genuinely free.
func IdleVictims(c Config, idleTimeout time.Duration, live []Candidate) []addr.Address
```

- [ ] **Step 1: Write the failing tests.** Cover:
  - under budget → `Victims` returns nothing
  - over budget → evicts cheapest-to-rewarm first, NOT simply least-recently-used: given two candidates with equal idleness, the one with the smaller `ContextTokens` goes first
  - a large-context bubble that is *recently active* is protected relative to an equally large one that is long idle
  - **budget is never blown to protect an expensive bubble** — if the only candidate left is expensive, it is still evicted
  - always-on and root are never returned as victims
  - `ContextTokens == 0` (unknown) is treated as a defined default, and the test pins which — an unknown must not accidentally become "infinitely precious" (never evicted) or "free" (always evicted first)
  - `IdleVictims` returns nothing for a bubble idle less than `CacheTTL`, even when it exceeds `idleTimeout` — evicting a warm cache for idleness alone is the waste this phase exists to stop
  - `IdleVictims` DOES return a bubble idle beyond both `CacheTTL` and `idleTimeout`
  - `CacheTTL == 0` disables the protection (falls back to today's behaviour exactly)

- [ ] **Step 2:** Confirm tests fail (package does not exist).

- [ ] **Step 3: Implement.** Score candidates by how wasteful evicting each would be — roughly rewarm cost against how long it has been idle — and evict the least wasteful first. Keep the scoring simple, total, and documented: a comment must state the intent in one sentence so a future reader can tell whether a change preserves it. Do NOT invent a pricing model; `ContextTokens` is the proxy for rewarm cost and that is sufficient.

- [ ] **Step 4:** Tests pass; `go test ./...` green.
- [ ] **Step 5: Commit** — `feat(paging): pure cost-aware eviction policy`

---

## Task 2: cache-TTL configuration

**Files:** Modify `internal/kernel/kernel.go` (a `CacheTTL` field beside `IdleTimeout`), `cmd/bubbles/app.go`.

- [ ] **Step 1: Write the failing test** — a kernel with `CacheTTL` set does not idle-evict a session idle less than `CacheTTL`; with `CacheTTL == 0` it behaves exactly as today.
- [ ] **Step 2:** Confirm it fails.
- [ ] **Step 3: Implement.** Add `CacheTTL time.Duration` to `Kernel` with a doc comment explaining that evicting inside this window discards a live prompt cache and converts a free wake into a paid one. Set it in `app.go` next to `IdleTimeout`, with a comment recording the assumed cache lifetime and that `IdleTimeout` below it is wasteful. **Do not silently change `IdleTimeout`'s value** — make the relationship explicit and let the configuration express it.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `feat(kernel): CacheTTL — do not evict a session whose prompt cache is still warm`

---

## Task 3: route eviction through the policy

**Files:** Modify `internal/kernel/kernel.go` (`EnforceBudget`, `EvictIdle`); tests alongside.

- [ ] **Step 1: Write the failing tests** — `EnforceBudget` evicts the cheap-to-rewarm bubble ahead of the expensive one at equal idleness; `EvictIdle` spares a warm-cache session; budget is still enforced when everything is expensive; always-on and root still exempt.
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Build `[]paging.Candidate` from the session table plus `Cost.Snapshot()` for `ContextTokens`, call the policy, apply the result. Keep the existing structure: gather under the lock, **measure `MemBytes()` outside the lock**, mutate under the lock, `Kill` outside. Increment `FEvictions` per eviction.
- [ ] **Step 4:** Tests pass; `-race` on `./internal/kernel/`.
- [ ] **Step 5: Commit** — `feat(kernel): cost-aware eviction — page out what is cheapest to rewarm`

---

## Task 4: count rewarms

**Files:** Modify `internal/kernel/kernel.go` (`EnsureAlive`); tests alongside.

- [ ] **Step 1: Write the failing test** — resuming a previously-evicted bubble increments `FRewarms`; a bubble that was never evicted does not.
- [ ] **Step 2:** Confirm it fails.
- [ ] **Step 3: Implement.** In `EnsureAlive`, when a session is relaunched for a bubble that had been paged out, increment `FRewarms`. This is the metric that tells the operator whether Phase 3 worked: evictions should stay flat while rewarms fall.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `feat(costmeter): count rewarms so eviction policy can be evaluated`

---

## Task 5: extract `internal/sessions` (behaviour-preserving)

**Files:** Create `internal/sessions/sessions.go` (+ test); modify `internal/kernel/kernel.go`.

**This task changes NO behaviour.** It is a move. Any behaviour change here is a defect.

- [ ] **Step 1:** Before touching anything, run the full suite and record the result — it is your baseline.
- [ ] **Step 2: Implement the move.** Extract the session table and its mechanics: `sessions` map, `lastUsed`, `clock`, `smu`, and the methods that operate on them (`session`, `setSession`, `touch`, `IsHot`, and the eviction plumbing). The kernel keeps orchestration and delegates. Keep exported behaviour identical; the kernel's public API must not change.
- [ ] **Step 3:** Run the full suite again. **Every pre-existing test must pass untouched.** If any fails, the move was not behaviour-preserving — fix the move, do not adjust the test.
- [ ] **Step 4:** Confirm `internal/kernel/kernel.go` shrank materially and report both line counts.
- [ ] **Step 5: Commit** — `refactor(sessions): extract the session table from kernel.go (no behaviour change)`

---

## Self-Review

**Spec §7 coverage:** cost-aware eviction → Tasks 1, 3; `IdleTimeout` vs cache TTL → Tasks 1, 2; `Evictions`/`Rewarms` validation → Tasks 3, 4; `internal/sessions` extraction → Task 5.

**Risk note:** Task 5 is the highest-blast-radius change in the phase and delivers no user-visible benefit on its own — it exists so Phase 4 and later work have a testable home for paging policy. It is deliberately sequenced LAST so that if it proves unsafe it can be dropped without losing Tasks 1–4.
