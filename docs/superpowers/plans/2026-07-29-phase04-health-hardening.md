# Phase 4: Health Hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the fleet's failure modes *visible* and *bounded*. Today a panic in any sweep kills the whole daemon, a bubble with a bad `Dir` relaunches forever re-paying a boot context each time, a wedged bubble is undetectable, and three files grow without limit.

**Architecture:** Extract a `internal/supervisor` package holding a **named check registry** that runs each check on its own interval, recovers panics per-check, and records status. Every one of the process's background loops migrates onto it. The registry then becomes the single source for the TUI's fleet-health block — no new pollers.

**Tech Stack:** Go, stdlib. Builds on Phase 0+1 (`cef78de`), Phase 2 (`1e0e780`), Phase 3 (`77468e0`).

**Spec:** `docs/superpowers/specs/2026-07-28-bubbles-cost-efficiency-design.md` §8, §8.1

**REQUIRED READING FOR EVERY TASK:** `.superpowers/sdd/phase04-grounding.md` — a cited, line-accurate survey of the current code. Every file:line in this plan comes from it. Read the sections relevant to your task before writing anything.

---

## Global Constraints

- **This is an upgrade, not a rewrite. Do not degrade anything.** Every behaviour that works today must still work.
- `go build ./... && go vet ./... && go test -count=1 ./...` green at every task boundary, plus `-race` on every touched package.
- **No existing test is deleted or weakened to make new code pass.** One test is a known, deliberate exception and is handled explicitly in Task 7 — every other failure means STOP and report the specific assertion.
- **`internal/supervisor` is pure policy + scheduling: no kernel import, no TUI import, no I/O of its own.** It runs `func(context.Context) error` closures the caller supplies. All time is caller-supplied or injected so tests are deterministic.
- **Lock order `notifyMu → Policy.mu → registry.mu`.** Never hold a lock across a PTY write, a `MemBytes()` probe, a launch, or a `Kill`.
- **`internal/sessions` stays a leaf** — it imports only `sync`, `internal/addr`, `internal/runner`. Do not add imports. Nothing that can block for an unbounded time may move into it.
- **Nothing on a background/sweep path may call `EnsureAlive`.** Waking a cold bubble pays a full prompt-cache rewarm — the cost this whole programme targets. The one pre-existing exception (`KeepAlive` for always-on receivers, `kernel.go:355`) is deliberate and stays; add no others.
- **Grace and CacheTTL are ordering preferences, never exemptions** (Phase 3). Nothing here may make the memory budget skippable.
- **`FRewarms` counts only genuine cold-cache wakes.** Backoff must not create a new path that inflates it.
- **Every suppression or silent-skip path increments a costmeter counter.** Silent suppression is a defect (Phase 1 law). Reuse the existing `F*` constants; do not invent duplicates for things already counted.
- **Degrade gracefully in the TUI:** a metric whose source is unavailable is OMITTED, never rendered as a zero. A zero reads as "verified healthy" when it means "not measured".
- File ceiling ~400 lines for new files.

---

## Task 1: `internal/supervisor` — named, panic-safe check registry

**Files:** Create `internal/supervisor/supervisor.go`, `internal/supervisor/supervisor_test.go`.

**Why:** `.superpowers/sdd/phase04-grounding.md` §1 — there are **17 background loops**, 13 of them the identical inline block in `cmd/bubbles/app.go:185-328`, and **there is no `recover()` anywhere in the repo**. A panic in any one of them takes down the entire daemon.

**Interfaces — Produces:**
```go
// Check is one named periodic job. Fn must be safe to call concurrently with
// nothing else in this package; the registry never calls the same Check twice
// at once.
type Check struct {
    Name     string
    Every    time.Duration
    Fn       func(context.Context) error
}

type Status struct {
    Name        string
    LastRun     time.Time
    LastErr     error     // nil = last run succeeded
    Consecutive int       // consecutive failures, 0 after a success
    Panicked    bool      // last run panicked (recovered)
    Runs        int64
}

type Registry struct{ /* unexported */ }

func New(now func() time.Time) *Registry
func (r *Registry) Register(c Check) error   // error on duplicate name, empty name, or Every <= 0
func (r *Registry) RunDue(ctx context.Context, at time.Time) // runs every check whose Every has elapsed
func (r *Registry) Snapshot() []Status       // sorted by Name, safe to call from the TUI goroutine
func (r *Registry) Failing() int             // checks whose last run failed or panicked
```

- [ ] **Step 1: Write the failing tests.** Cover:
  - a registered check runs when due and not before (`RunDue` with a supplied `at`, no real clock)
  - **a check that panics does not propagate**: `RunDue` returns normally, the panic is recorded as `Panicked: true` with a non-nil `LastErr`, and **every other registered check still runs in the same `RunDue` call** — this is the whole point of the task
  - a check that panics on one run and succeeds on the next clears `Panicked` and resets `Consecutive` to 0
  - a returned error is recorded and increments `Consecutive`; a success resets it
  - duplicate names, empty names and `Every <= 0` are rejected by `Register` with an error
  - `Failing()` counts exactly the checks whose last run failed or panicked
  - `Snapshot()` is sorted by name and returns a copy — mutating the result cannot affect the registry
  - a check that respects `ctx` cancellation is not re-run after the context is done

- [ ] **Step 2:** Confirm the tests fail (package does not exist).

- [ ] **Step 3: Implement.** One mutex guarding the check table and statuses. `RunDue` must release the lock before invoking a check's `Fn` — a check will do I/O and must never run under the registry lock. Recover per check, converting the recovered value to an error that includes the check name and the stack. Do not swallow it silently; the recorded `Status` is the report.

- [ ] **Step 4:** Tests pass; `go test -race ./internal/supervisor/`.
- [ ] **Step 5: Commit** — `feat(supervisor): named, panic-safe periodic check registry`

---

## Task 2: migrate every background loop onto the registry

**Files:** Modify `cmd/bubbles/app.go` (the 13 inline blocks at `:185-328`), `cmd/bubbles/health.go`, and the `for range t.C` tails in `runSampler` / `runClaudeUsage` / `runHeadroomStats`. See grounding §1 for the exact list, intervals and call targets.

**This task changes NO behaviour** beyond becoming panic-safe: every loop keeps its current interval and calls the same function it calls today. Any interval change is a defect.

- [ ] **Step 1:** Run the full suite and record it as your baseline.

- [ ] **Step 2: Write the failing test** — a test asserting that every expected check name is registered with the interval it has today. Name the checks after what they do (`"budget"`, `"idle"`, `"health-sweep"`, `"sampler"`, …). This test is the migration's completeness gate: it must enumerate the checks explicitly, so a loop left behind is a failure, not an omission nobody notices.

- [ ] **Step 3: Implement.** Replace the inline goroutine blocks with `Register` calls plus **one** driver goroutine calling `RunDue` on a short tick (1s). Preserve every interval verbatim from the current code — copy them, do not retype from memory. The three `for range t.C` tails inside `runSampler`/`runClaudeUsage`/`runHeadroomStats` migrate the same way; keep their setup code where it is and register only the periodic body.

- [ ] **Step 4:** Full suite green, and it must match the Step 1 baseline exactly. Manually confirm the process still starts and the TUI still updates.
- [ ] **Step 5: Commit** — `refactor(app): run every background sweep through the supervisor registry`

---

## Task 3: crash-loop backoff in `EnsureAlive`

**Files:** Modify `internal/kernel/kernel.go` (`ensureAlive`, `clockNow`), `internal/kernel/fakes_test.go` (or wherever `FakeRunner` lives), tests alongside.

**Why:** grounding §2 — `EnsureAlive(a addr.Address) runner.Session` has **no failure counter and no terminal state**. A bubble with a nonexistent `Dir` burns ~3.3s of `time.Sleep` per attempt (2.5s `resumeProbeWindow` + 0.8s `RelaunchProbe`), forever, re-paying a boot context each time.

**Model it on** `headroomProc.supervise` (`cmd/bubbles/headroom.go:124-148`), which already does this correctly: a failure counter, reset on success, and a terminal give-up after 5 (grounding §3). Note that its "backoff" is a retry *budget* with a flat probe, not a growing delay — for `EnsureAlive` use a **growing** delay, because the cost here is a re-paid boot context, not a cheap signal probe.

**Prerequisites this task must also do:**
- Convert `clockNow` from a method returning `time.Now()` into an injectable `now func() time.Time` field, defaulted in `New`. Grounding §10: its own comment claims tests could stub it, but it is a method, so nothing can. Backoff tests need determinism.
- Add a `FailLaunch` knob to `FakeRunner`, whose `Launch` currently **always returns nil** (grounding §10). This is an additive change to a fake used by ~100 tests: default behaviour must be unchanged.

- [ ] **Step 1: Write the failing tests.** Cover:
  - a bubble whose launch fails is not retried immediately: a second `EnsureAlive` within the backoff window returns without attempting a launch
  - the delay grows across consecutive failures and is capped
  - **a successful launch resets the counter and the delay completely** — a bubble that failed 4 times and then succeeds is back to normal
  - after the terminal threshold the bubble stops being relaunched and the state is *reported*, not silent
  - **backoff must not inflate `FRewarms`**: a suppressed retry counts nothing, and a failed launch counts nothing (Phase 3 law — `FRewarms` counts only genuine cold-cache wakes)
  - always-on receivers are subject to the same backoff — a crash-looping always-on bubble is exactly the worst case, and `KeepAlive` touching it every sweep must not defeat the counter

- [ ] **Step 2:** Confirm they fail.

- [ ] **Step 3: Implement.** Per grounding §5, the counter **cannot live in `internal/sessions`** — that package is a leaf whose mutex must never span a launch. Give it its own kernel-level mutex, read *before* the launch and written *after* it, following the existing `HealthManager.pumpMu`/`lastPump` precedent. Do not hold it across the launch.

- [ ] **Step 4:** Tests pass; `-race` on `./internal/kernel/`.
- [ ] **Step 5: Commit** — `feat(kernel): crash-loop backoff so a bad Dir stops re-paying a boot context`

---

## Task 4: stuck-bubble detection

**Files:** Create `internal/health/stuck.go` + test (a pure detector), register it as a supervisor check in `cmd/bubbles/`.

**Why:** grounding §4. Read it carefully before designing — **the obvious signal does not work:**
- `InputReady` is a **one-way latch**: an `atomic.Bool` that `readyWatcher` only ever stores `true`, including on boot-deadline timeout and on process death. It means "was ready once", not "is ready now". **A bubble wedged on a permission prompt still reports `InputReady() == true`. Do not use it for stuck detection.**
- `LastActivity()` is *output* activity and falls back to launch time, so a spinner or a redrawing prompt reads as maximally warm. It cannot distinguish "working" from "wedged" on its own.

**Therefore the detector is a pure function over a snapshot, and it must be conservative.** A bubble is reported STUCK when it has work it has not consumed and has produced no *new* output for a threshold — concretely, unread notifiable mail plus a `LastActivity` older than the threshold, or a `RecentOutput` that has not changed across consecutive samples while mail is pending.

**This task REPORTS ONLY. It must not kill, restart, or notify any bubble.** Auto-remediation is out of scope: a false positive that kills a working bubble is far worse than a missed detection. The output feeds the TUI panel (Task 7) and nothing else.

**Interfaces — Produces:**
```go
type Sample struct {
    Addr          addr.Address
    LastActivity  time.Time
    RecentOutput  string
    UnreadMail    int
    Alive         bool
}
type Config struct {
    Threshold  time.Duration // no new output for this long, with mail pending
}
// Stuck returns the addresses that look wedged, given the previous sample set
// and the current one. Pure: all time is caller-supplied.
func Stuck(c Config, prev, cur []Sample, now time.Time) []addr.Address
```

- [ ] **Step 1: Write the failing tests.** Cover:
  - a bubble with pending mail and unchanged output past the threshold is reported
  - a bubble with pending mail whose output CHANGED between samples is NOT reported, however long it has been running — that is a working bubble
  - a bubble with no pending mail is never reported, however quiet — that is an idle bubble, which is `EvictIdle`'s business, not this one
  - a bubble not `Alive` is never reported
  - a bubble seen for the first time (absent from `prev`) is never reported — one sample can never establish "unchanged"
  - the threshold boundary is pinned exactly (at the threshold vs one nanosecond under)
  - the known permission-prompt / resume-menu strings from grounding §4 (`"No conversation found"`, `resumeMenuOpt1`, `resumeMenuOpt2`) do not by themselves trigger a report — quote them verbatim from their current definitions rather than retyping

- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Pure, no `time.Now()`, no kernel import.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `feat(health): conservative stuck-bubble detection (reports only)`

---

## Task 5: disk caps

**Files:** Modify wherever each file is opened; see grounding §5 for exact sites and flags.

**Three leaks, in order of real-world severity — note this differs from the spec, which named only the first two:**

1. **`.bubbles/daemon.log` is the actual problem.** Opened `O_APPEND` with **no** `O_TRUNC` (`client.go:209`), never rotated, and it swallows the daemon's entire stderr — every throttled warning, ngrok's output, everything. It is also the file the TUI's own error flash tells users to check, so it must stay readable and must not be silently emptied.
2. **`.bubbles/headroom.log`** — `O_TRUNC` per launch, so bounded across restarts but unbounded within a long run.
3. **Temp configs, never removed by anything** — `os.TempDir()/bubbles-mcp-<pid>-<addr>.json` (`internal/runner/local.go:178`) **and** `bubbles-settings-<pid>-<addr>.json` (`local.go:105`), which the spec missed. Both are written on **every** Launch.

- [ ] **Step 1: Write the failing tests.** Cover:
  - a size-capped writer rotates at the cap and keeps the most recent content, not the oldest — a log truncated to its first N bytes is useless
  - rotation keeps exactly one previous generation (`.log` + `.log.1`), so total disk is bounded by 2× the cap
  - a write larger than the cap does not wedge or infinitely recurse
  - temp-config cleanup removes files matching the fleet's own pid pattern and **leaves other processes' files alone** — a stale-file sweep that deletes a live daemon's config is a self-inflicted outage
  - cleanup is safe to run repeatedly (idempotent) and safe when the directory does not exist

- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** One small size-capped rotating writer used by both logs. Remove each temp config when its session is killed, and register a periodic sweep (via the Task 1 registry) for files this process orphaned. Match strictly on the current pid pattern.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `feat(app): bound daemon.log, headroom.log, and the per-launch temp configs`

---

## Task 6: periodic verifier reap + expired mute-rule cleanup

**Files:** Modify `cmd/bubbles/app.go:214` (the boot-only `ReapOrphanVerifiers` call), `internal/notify/rules.go`.

**Why:** grounding §6 — `ReapOrphanVerifiers` runs **once at boot**, so tasks completing during a long run leave verifier bubbles resident until restart. Grounding §7 — mute rules **already carry `TTL` and `Created`**, and expiry is already enforced in `Compiled.Match`, but **nothing ever removes an expired rule**, so the rule set only grows.

**Read grounding §7 before touching the rules:** `internal/notify/rules.go:121-131` explicitly forbids moving the expiry check into `Add`. Respect that; add a separate reap, do not relocate the existing check.

- [ ] **Step 1: Write the failing tests.** Cover:
  - reaping expired rules removes exactly the expired ones and leaves live ones and TTL-less ones untouched
  - **reaping is observationally invisible to `Match`** — for every input, `Match` returns the same answer before and after a reap. The reap is a memory cleanup, never a behaviour change. If this test is hard to write, the reap is doing too much.
  - the reap respects `MaxRules` bookkeeping so a bubble can add a rule again after its old ones expire
  - the verifier reap is idempotent and safe to call on the periodic path

- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Register both as named checks on the Task 1 registry. Before wiring the verifier reap periodically, satisfy yourself from grounding §6 that it is idempotent, that its locking is compatible with the sweep path, and **that it cannot race a task completing mid-reap** — if it can, fix that first and say so in your report. It must not call `EnsureAlive`.
- [ ] **Step 4:** Tests pass; `-race` on touched packages.
- [ ] **Step 5: Commit** — `feat(app): reap orphan verifiers and expired mute rules periodically, not just at boot`

---

## Task 7: complete the fleet-health panel

**Files:** Modify `internal/tui/view.go` (`fleetHealthRows`, `fleetHealthSnapshot`), `internal/tui/view_test.go`.

**Why:** spec §8.1. Grounding §8 — `fleetHealthRows` currently renders 2-3 rows (`FLEET · H/T hot`, `mute/cap/inline`, conditional `backlog`) from `fleetHealthSnapshot` on the 2s sampler. It **deliberately omits** stuck / crash-loop / failing-check counts because Phase 4 had not yet measured them.

Add, in the spec's priority order (which is also the drop order when the panel is width-constrained): stuck count, crash-looping count, bubbles over the context threshold, failing checks. Sources: Task 4's detector, Task 3's backoff state, Phase 2's `FContextTokens`, and Task 1's `Registry.Failing()`.

**Rendering requirements (spec §8.1):** reuse the existing `panelHead` / `panelStyle` / `sevStyle` — this is a new block in an established panel, not a new visual language. Severity via `sevStyle`: green when all checks pass and nothing is stuck; amber for a context-threshold or backlog warning; red for a stuck bubble, a crash loop, or a failing check. **Any metric whose source is unavailable is omitted, never zeroed.**

**⚠️ THE ONE SANCTIONED TEST CHANGE IN THIS PHASE.** `TestFleetHealthRowsOmitUnavailableMetrics` (`internal/tui/view_test.go:39`) asserts the rendered output does **not** contain `"stuck"`. That was correct while stuck-ness was unmeasured; Task 4 measures it, so the test's premise is superseded — this is the case the Global Constraints reserve. **Do not delete it and do not weaken it.** Replace it with its successor, which must assert BOTH directions:
  - when the snapshot carries no stuck data (source unavailable), the row is still omitted — the original guarantee, preserved
  - when the snapshot carries a stuck count, the row IS rendered
A test that only checks the second direction has silently dropped the guarantee the original existed to hold.

- [ ] **Step 1: Write the failing tests.** Cover: each new metric rendered from a snapshot; each omitted when its source is absent; the severity colour chosen for each of green/amber/red; the width-constrained drop order; and the replacement guard test above. Render functions are tested without a terminal — follow the existing conventions in grounding §10.
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** A pure render function over a snapshot struct, per spec §8.1 and the §3.2 extraction discipline. No new pollers — everything renders from state the fleet already maintains.
- [ ] **Step 4:** Tests pass; full suite green.
- [ ] **Step 5: Commit** — `feat(tui): fleet health in the top-right panel — stuck, crash loops, failing checks`

---

## Self-Review

**Spec §8 coverage:** panic-safe supervisor + named registry → Tasks 1, 2; crash-loop backoff → Task 3; stuck-bubble detection → Task 4; disk caps → Task 5; periodic verifier reap → Task 6; expired mute-rule cleanup → Task 6; health surface in the TUI → Task 7. **§8.1 coverage:** all six metrics, the style reuse, the severity rules, the drop order, and the omit-don't-zero rule → Task 7.

**Scope additions beyond the spec, deliberate and recorded:** `.bubbles/daemon.log` rotation and `bubbles-settings-<pid>-<addr>.json` cleanup (Task 5) — both found by the grounding survey, both strictly worse leaks than the two the spec named. `clockNow` injectability and `FakeRunner.FailLaunch` (Task 3) — prerequisites without which the backoff cannot be tested deterministically.

**Risk note:** Task 2 is the highest-blast-radius change (it touches every background loop in the process) but is behaviour-preserving and gated by an explicit completeness test. Task 3 is the highest-subtlety change: the backoff counter sits outside every existing lock, and getting that wrong reintroduces exactly the kind of check-then-act logic race that `-race` cannot see and that an earlier phase already shipped once.
