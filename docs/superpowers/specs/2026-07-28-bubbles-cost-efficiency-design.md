# Bubbles Cost & Efficiency Overhaul — Design

**Date:** 2026-07-28
**Status:** Approved for implementation
**Scope:** Five phases (0–4) plus three staged package extractions.

---

## 1. Problem

A bubble pays for its entire context on **every turn**. Fleet cost is therefore
approximately:

```
cost  ≈  Σ over bubbles Σ over turns ( context_size × cache_miss_factor )
```

Three levers follow directly: **context size**, **turn count**, and **cache hit
rate**. The current system optimises none of them deliberately. It optimises
resident memory (`EnforceBudget`, `EvictIdle`) — a proxy that is sometimes
actively anti-correlated with token cost.

### Observed live leak

A bubble receiving `opt_out` events from the `mechmagnet-event-pump` webhook has
been woken on every event for roughly four days. Each event costs a wake, a
typed notice, an `inbox()` tool call, and a reasoning turn on a large context —
to reach the conclusion "this is noise" every single time. The bubble is
hand-maintaining an event-id watermark in model turns to do what the kernel
could do for free.

Root cause: `cmd/bubbles/webhook.go:95` declares `Urgent *bool` with the
comment *"default true: a programmatic poke should wake the bubble"*, and
`webhook.go:126` mirrors it (`urgent := q.Get("urgent") != "false"`). The
existing `rateLimiter` (1/s sustained, burst 20, per token) protects the HTTP
server from floods; it does nothing about a steady low-rate stream forever.

### Secondary defect in the same trace

Two notices landed for one message (`📬 New message from webhook…` followed by
`📬 You have 1 unread message(s)…`), and the receiving bubble burned reasoning
on *"could be a real message"*. `markNudge` and `recoverNudge` guard the
`notified`/`lastNudge` maps with `notifyMu`, but the `Store.UnreadCount` read
and the subsequent PTY write happen outside that lock, in two different
goroutines (the send path and the 45 s `RecoverUnread(true)` sweep). Suspected
TOCTOU; to be confirmed with a reproducing test before the fix lands.

---

## 2. Hard constraint from history

Commit `632fe95` reverted three prior commits (`3cfb0d9`, `bf286be`, `33e5ead`)
that attempted this exact nudge-dedup/recovery work. They caused a **fleet-wide
redelivery flood: a single already-delivered event re-emitted 100–178×**. The
revert message states the real driver was the fleet dying on VM/SSH disconnect
(daemon in a login-session cgroup scope), fixed separately in `9c7bcb9`, and
that "the nudge fix will be re-approached carefully after".

This work is that re-approach. Two non-negotiable consequences:

**INV-1 — Flood ceiling.** Independent of all policy, the system MUST enforce an
absolute cap on notices written to any one bubble per unit time. This is a
backstop that sits *below* the policy engine and cannot be disabled by any rule,
capability, or configuration. If policy ever says "notify" more often than the
ceiling, the ceiling wins and the excess is counted and reported, not delivered.

**Default: 6 notices per bubble per minute, token-bucket with burst 6.** The
observed flood was 100–178 redeliveries of one event; this ceiling caps that at
6 and records the remainder in `NoticesCapped`. The number is a starting value
to be revised against Phase 0 data, but it is never unbounded and never
configurable to unbounded.

**INV-2 — Dedup state must be correct across relaunch.** The reverted work
oscillated between "dedup survives relaunch" (causing multi-hour delivery
stalls) and "dedup cleared on relaunch" (causing the flood, because a fleet that
was restarting constantly re-nudged on every restart). The correct model is that
notification state is keyed to the *message backlog*, not to the session
lifetime, and is reconciled from the store rather than reset. Any change here
ships with regression tests for both failure directions.

---

## 3. Target architecture

### 3.1 Current structural problems

| Problem | Evidence |
|---|---|
| Kernel god object | `internal/kernel/kernel.go` is 1301 lines holding five unrelated responsibilities behind four mutexes: session table (`sessions`/`lastUsed`/`clock`/`smu`), notification state (`notified`/`lastNudge`/`notifyMu`), focus/typing (`lastKey`), message routing, and lifecycle/capability orchestration. |
| Untestable delivery policy | Policy is a ~30-line comment wrapped around a nested if/else in `deliverMessage`, partly duplicated in `RecoverUnread`. Fused to a PTY write, so it cannot be unit-tested. This is the code that caused the 178× flood. |
| Unmanaged background work | Twelve anonymous ticker goroutines in `cmd/bubbles/app.go` with no names, no panic recovery, no shutdown path, no last-run visibility. |
| Health daemon not reusable | `HealthManager` lives in `package main` (`cmd/bubbles/health.go`), so it cannot be tested or reused independently of the binary. Its `Sweep()` runs exactly one check. |
| No per-bubble cost telemetry | `claudeusage.go` fetches account-level OAuth quota (fleet-wide, unattributable). `headroomstats.go` reports proxy-level compression savings. Neither can answer "did this change reduce bubble 0.4's turn count?" |

### 3.2 Three staged extractions

Each extraction is performed by the phase that already rewrites that code, so
its risk is absorbed by work being done anyway. **No big-bang refactor.**

**`internal/notify` — extracted in Phase 1.** A pure policy engine with no PTY,
no kernel reference, and no I/O:

```go
type Action int // Suppress | Notice | Inline | Rollup

type Decision struct {
    Action Action
    Text   string // rendered notice, empty when Suppress
    MarkRead []int // message ids consumed by an Inline delivery
}

func (p *Policy) Decide(msg Message, st State, now time.Time) Decision
```

The kernel calls `Decide` and then performs exactly one action with the result.
Mute predicates, throttle windows, rollups, coalescing, and inline-injection
thresholds all become table-driven unit tests. The flood ceiling (INV-1) is
enforced inside `Policy` and covered by its own tests.

**`internal/sessions` — extracted in Phase 3.** The session table plus `IsHot`,
`touch`, `EnsureAlive`, `EnforceBudget`, `EvictIdle`, `KeepAlive`,
`RelaunchSession`. Phase 3 makes eviction token-cost-aware; that policy needs a
real home with real tests rather than being methods on a 1301-line struct.

**`internal/supervisor` — extracted in Phase 4.** A registry of named periodic
checks:

```go
type Check struct {
    Name   string
    Every  time.Duration
    Jitter time.Duration
    Run    func(context.Context) error
}
```

with per-check panic recovery, error capture, last-run/last-error stats, and
context cancellation for clean shutdown. Every periodic loop registers here.
`HealthManager` becomes registered checks and its bespoke `Run` is deleted.
Phase 4 observability falls out for free: the TUI renders the registry.

### 3.3 File-size discipline

No file introduced or substantially rewritten by this work may exceed ~400
lines. Where an existing file is already over that and is being modified, the
modification extracts rather than appends. `kernel.go` is expected to fall
below ~800 lines by the end of Phase 3.

---

## 4. Phase 0 — Cost telemetry (`internal/costmeter`)

**Built first.** Without per-bubble telemetry, Phases 1–3 are unfalsifiable — a
win and a regression are indistinguishable.

Per-bubble counters, cheap to record and cheap to read:

- `TurnsTriggered` — notices written that plausibly caused a turn
- `NoticesWritten`, `NoticesSuppressed` (by rule), `NoticesCapped` (by INV-1)
- `DeliveriesInline` vs `DeliveriesViaTool`
- `Evictions`, `Rewarms`
- `ContextTokens` — latest observed context size (source: Phase 2)

Requirements:

- In-memory, versioned like `inbox.Store` so the existing `startSaver` pattern
  persists it without a new mechanism.
- Read-only snapshot API for the TUI and for `bubbles` CLI output.
- No network calls. This measures *our* behaviour, not the provider's billing.

**Exit criterion:** counters visible per bubble, and a before/after comparison
is possible for every subsequent phase.

---

## 5. Phase 1 — Notification economics

All five items touch the same code path (`deliverMessage`, `formatNotify`,
`markNudge`/`recoverNudge`, `RecoverUnread`, `handleWebhook`). They ship
together because splitting them would mean rewriting those ~40 lines five
times.

### 5.1 Mute rules (the new primitive)

A bubble manages its own rules through new MCP tools:

```
mute(source?, subject_re?, body_re?, window, ttl?) -> rule_id
unmute(rule_id)
mutes()
```

**Matching (predicate form "D").** `source` exact-match, `subject_re` and
`body_re` are regular expressions. All supplied fields are AND-ed; omitted
fields match anything. At least one field must be supplied.

Go's `regexp` is RE2 — linear time, no backtracking — so catastrophic-backtrack
DoS is not reachable. Bounds are still enforced:

- Patterns compiled at **rule-creation** time; a compile error is returned to
  the calling bubble immediately and the rule is never stored.
- Pattern length ≤ 512 bytes.
- `body_re` is matched against the **first 4 KB** of the body only.
- ≤ 32 rules per bubble.
- Compiled regexes cached, keyed by pattern string.

**Evaluation happens at file time, not at ingest.** The message is *always*
stored. Only the terminal write is gated. A muted message is never lost — it is
read on the bubble's next `inbox()` call for any reason.

**Data model.** `inbox.Message` gains `Muted bool`, set when a rule matches.
`Store.UnreadCount()` remains truthful (used by `inbox()`); a new
`Store.NotifiableCount()` counts unread-and-not-muted and drives the notify
decision.

**Window semantics.** The first match delivers normally and opens the window.
Matches inside the window are filed silently. When the window expires, the next
match delivers a **rollup** — `"14× opt_out since 14:02"` — and reopens the
window. The bubble is throttled, never permanently blind.

**Persistence.** Rules live on the registry entry and persist through
`fleet.json`, alongside the existing `AlwaysOn` and `Disabled` fields.

**TTL.** Optional. An expired rule is removed by a Phase 4 health check.

### 5.2 Flip the webhook urgent default — DESIGNED, NOT IMPLEMENTED IN THIS PASS

`webhook.go:95` and `webhook.go:126`. Programmatic pokes would pool by default;
`urgent=true` would become explicit opt-in.

**Deferred by operator decision (2026-07-28).** This is a breaking change to the
webhook contract — any existing caller relying on the implicit wake goes silent
until it sets `urgent=true`. Rishi is auditing the live pumps first. The design
stays in this spec; implementation is gated on that audit and lands as a
follow-up.

**Consequence for §5.1 — mute must gate the wake, not just the notice.** In
`deliverMessage`, `urgent` calls `EnsureAlive(to)` (paging a cold bubble in)
*before* any notification decision is reached. If a mute rule suppressed only
the typed notice, a muted `urgent=true` webhook would still wake the bubble on
every event and pay a full prompt-cache rewarm — the dominant cost (§7). Since
webhooks remain urgent-by-default for now, this is not a corner case; it is the
`mechmagnet` case.

Therefore: **mute evaluation happens before the urgent wake decision.** A
message matching an active mute rule inside its throttle window does not page in
a cold bubble, does not touch LRU, and does not write to the PTY. It is filed
and read on the bubble's next `inbox()` call. Mute overrides `urgent`; this is
deliberate, because a mute rule is the bubble's own explicit statement that the
traffic is noise, and only the bubble can know that.

`AlwaysOn` receivers are exempt from mute-suppressed wakes only in the sense
that they are already hot — no page-in occurs either way, so the notice
suppression still applies and still saves the turn.

### 5.3 Direct injection of small bodies

When a message's body is under a length threshold and there is no larger
backlog, inline the body into the notice and mark the message read — saving one
turn and one tool call per message.

**Defaults:** inline when the sanitised, newline-flattened body is ≤ 280 bytes
and the notifiable backlog is ≤ 3 messages (all of which are inlined together).
Above either bound, fall back to the existing notice + `inbox()` path. These are
starting values, tuned against Phase 0 data.

**Security requirement.** `kernel.go:1289` documents that bodies are
deliberately *not* sanitised because they are read via a tool and never typed.
Inlining makes bodies typed input for the first time, re-opening the PTY
injection vector (one agent puppeting another). Therefore: `sanitizePTY` MUST be
applied to any inlined body, and newlines flattened, before it reaches the PTY.
This is a blocking requirement, not a nicety.

### 5.4 Coalescing window

A debounce so a burst of messages becomes one notice carrying several bodies.
Composes with 5.3. Must respect INV-1.

**Default: 3 s window**, with an immediate-flush exception for `urgent=true`
(an urgent message is never held for coalescing — that would defeat its
purpose). Starting value, tuned against Phase 0 data.

### 5.5 Fix the double-notice race

A single `shouldNotify(to)` helper used by both the send path and
`RecoverUnread`, performing the count read and the notify decision under one
lock. Ships with a regression test that reproduces the double-notice.

**Exit criteria:** measured reduction in `TurnsTriggered` for a webhook-heavy
bubble; no notice sequence exceeds the INV-1 ceiling under a synthetic flood
test; regression tests cover both INV-2 failure directions.

---

## 6. Phase 2 — Context economics

**Token-count source.** The transcript `.jsonl` already walked by
`trimTranscripts` carries per-assistant-message usage. The most recent entry's
cumulative input tokens *is* the current context size. Read off-process, no PTY
scraping.

**Tiered compaction pump.**
- ≥ 500 k — inject a notice: *"context at N — call `compact()` at your next
  checkpoint"*. Subject to INV-1 and to its own throttle so it is not repeated
  every sweep.
- ≥ 800 k — kernel calls `Compact()` directly.

`ResumeSummaryThreshold` is already `500_000`; the constant is shared, not
duplicated.

**Fix `trimTranscripts` blind spot.** `latest < 0 → return nil` means a bubble
that has *never compacted* is never trimmed — precisely the runaway case. Add a
byte-ceiling trim path for never-compacted transcripts, preserving the existing
safety rule that only **cold** bubbles are rewritten.

**Gate the citizen prompt on capability.** MCP tool *schemas* are already gated
(`app.go:117` → `Caps.CanSpawn`), but `citizenPrompt` describes
`spawn`/`edit`/`delete`/`introduce`/`broadcast`/`assign_task` in prose to every
bubble regardless. A leaf worker pays for roughly 40 % of a 6.4 KB prompt, on
every turn, describing tools it does not have. Gate the prose to match the tool
gating.

---

## 7. Phase 3 — Cache-aware paging

Paging is not free; it converts a RAM cost into a token cost. A paged-out bubble
resumes with a cold prompt cache and re-pays full uncached input on its first
turn back. Evicting a 600 k-context bubble to reclaim RAM and waking it 90
seconds later can cost more in one rewarm than the RAM was worth.

`EvictIdle` and `EnforceBudget` currently consider resident bytes only.

**Changes:**
- Eviction becomes cost-aware: prefer evicting small-context bubbles; strongly
  protect large-context bubbles that were recently active.
- `IdleTimeout` is chosen relative to the prompt-cache TTL rather than for
  memory reasons alone.
- `Rewarms` and `Evictions` counters (Phase 0) validate the policy.

Extraction of `internal/sessions` happens here.

---

## 8. Phase 4 — Health hardening

- **Panic-safe supervisor** with a named check registry (§3.2). Every existing
  ticker migrates.
- **Crash-loop backoff** in `EnsureAlive`. Today a bubble with a bad `Dir`
  relaunches on every touch forever, re-paying a full boot context each time.
  There is no failure counter and no terminal state. Model it on
  `headroomProc.supervise`, which already does this correctly (probe, backoff,
  give up after 5).
- **Stuck-bubble detection.** `LastActivity()` is currently consumed only by
  `EvictIdle` (quiet ⇒ page out). There is no detection of a bubble that is hot
  and wedged — parked on a permission prompt, holding an unsubmitted line, or
  spinning. `RecentOutput()` already exists and is pattern-matched for exactly
  two strings today.
- **Disk caps.** `.bubbles/headroom.log` truncates only at launch (`O_TRUNC`) and
  grows unbounded during a long run. `os.TempDir()/bubbles-mcp-<pid>-<addr>.json`
  is written on every launch (`internal/runner/local.go:178`) and never removed.
- **Periodic verifier reap.** `ReapOrphanVerifiers` runs once at boot
  (`app.go:214`); tasks completing during a long run leave verifier bubbles until
  the next restart.
- **Expired mute-rule cleanup** (from §5.1 TTL).
- **Health surface** in the TUI, rendered from the check registry. Today every
  failure path is `fmt.Fprintf(os.Stderr, …)`, which a Bubble Tea TUI swallows.
  See §8.1.

### 8.1 Fleet-health panel (top right, alongside usage)

The top-right overlay already exists: `usagePanel` (`internal/tui/view.go:146`),
pinned by `overlayTopRight` (`:170`, applied at `:495`). It renders three blocks
in order — Claude account usage (`claudeUsageRows`), headroom compression
savings (`headroomRows`), and `RESOURCES · N hot` with RAM/CPU and top
consumers.

Fleet health becomes a **fourth block in the same panel**, so cost and health
are read together. Data comes from the Phase 0 costmeter and the Phase 4 check
registry — no new pollers; it renders from state the fleet already maintains.

Numbers to surface, in priority order (the panel is width-constrained, so this
is also the drop order when space is short):

| Metric | Source | Why it earns its space |
|---|---|---|
| `hot/total` bubbles | session table | already partly present as `N hot` |
| stuck / crash-looping count | Phase 4 detectors | the failure modes that are invisible today |
| bubbles over context threshold | Phase 2 token counts | predicts the next expensive rewarm |
| notices suppressed · capped | Phase 0 costmeter | proves the mute rules and INV-1 are working |
| failing checks | supervisor registry | a dead sweep is currently undetectable |
| total unread backlog | inbox store | catches a bubble ignoring its mail |

Rendering requirements:

- Reuse the existing `panelHead`/`panelStyle`/`sevStyle` styles — this is a new
  block in an established panel, not a new visual language.
- Severity colouring via `sevStyle`: green when all checks pass and nothing is
  stuck; amber for a context-threshold or backlog warning; red for a stuck
  bubble, a crash loop, or a failing check.
- Degrade gracefully: any metric whose source is not yet implemented (earlier
  phases not landed) is omitted, not rendered as zero.
- Pure render function over a snapshot struct, testable without a terminal —
  consistent with the extraction discipline in §3.2.

A follow-up may add a drill-down view listing per-check status, but the panel
itself stays a fixed-height summary.

---

## 9. Testing strategy

- `internal/notify`, `internal/costmeter`, `internal/sessions` are pure and get
  table-driven unit tests. No PTY required.
- **Flood test (INV-1):** synthetic burst of N messages to one bubble asserts
  notices written ≤ ceiling. This is the regression gate for `632fe95`.
- **Both INV-2 directions:** a test that a backlog surviving a relaunch is still
  announced, and a test that a relaunch does not re-announce an
  already-announced backlog.
- **Double-notice regression** (§5.5) reproduced before it is fixed.
- **Sanitisation test (§5.3):** a body containing escape sequences must not
  reach the PTY unescaped.
- Existing `go test ./...` must stay green at every phase boundary.

### 9.1 Non-regression discipline

The explicit operator requirement is *upgrade, not degrade*. Every phase
observes the following, and a phase that cannot is not merged:

1. **No existing test is deleted or weakened to make new code pass.** If an
   existing test must change, the behaviour change is intentional, called out,
   and approved — not folded silently into an implementation commit.
2. **Extractions are behaviour-preserving by construction.** `internal/notify`,
   `internal/sessions`, and `internal/supervisor` move logic; they do not change
   it. Behaviour changes land in separate commits from the moves, so a
   regression bisects to an obvious culprit.
3. **Every new suppression path is reversible and observable.** Anything that
   stops a message, a notice, or a wake increments a Phase 0 counter. Silent
   suppression is a defect — the failure mode of `632fe95` was invisible
   redelivery, and the mirror failure (invisible non-delivery) is worse.
4. **No message is ever dropped.** Mute, coalescing, and inlining affect
   *notification*, never storage. `Store.UnreadCount()` stays truthful and every
   filed message remains readable via `inbox()`.
5. **Each phase leaves the tree green, shippable, and independently
   revertable.**

---

## 10. Non-goals

- **Model tiering.** Forcing `claude-opus-5[1m]` fleet-wide
  (`internal/runner/local.go:166`) is **already the cost-optimal choice**, per
  operator (2026-07-28): opus-5 matches fable on performance at a lower rate,
  and sonnet-5's higher token consumption cancels its lower per-token price,
  making it no cheaper in practice. Tiering is therefore not a lever left
  unpulled — it is closed on purpose. The setting is trivially reversible in
  `opusOneM` if the economics change.
- Rewriting the TUI.
- Changing the task/verification protocol beyond steering assigners toward
  `check_cmd` (which is token-free) over checklist verifier bubbles.
- Any refactor not required by a phase in this document.

---

## 11. Sequencing

```
Phase 0  costmeter                    (enables measurement of everything below)
Phase 1  notification economics       + extract internal/notify
Phase 2  context economics
Phase 3  cache-aware paging           + extract internal/sessions
Phase 4  health hardening             + extract internal/supervisor
```

Each phase leaves the tree green and shippable.
