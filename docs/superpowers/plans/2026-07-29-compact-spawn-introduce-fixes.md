# Fix: compact() never runs, spawn() returns phantom addresses, introduce() accepts them

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make three kernel promises true. `compact()` says it will run and never does. `spawn()` returns an address for a bubble that is not durably created. `introduce()` accepts addresses for bubbles that do not exist.

**Architecture:** Three independent fixes, smallest blast radius first. No new packages; the one new mechanism (deferred compact) reuses the existing supervisor check registry and the `LastActivity` idle signal already used by `internal/health`.

**Tech Stack:** Go, stdlib. Branch `fix-compact-spawn-introduce` off main at `c3a78d4`.

**Reported by:** bubble `0.9` (claude_business), independently observed by `0.9.34` and `0.9.30`. Two bubbles lost; one lane deliberately frozen. Verbatim report is in this repo's ledger for this plan.

---

## Global Constraints

- **Upgrade, not degradation.** Every behaviour that works today must still work.
- **No message is ever dropped.** `Store.UnreadCount()` stays truthful.
- **Nothing on a background/sweep path may call `EnsureAlive`.** Waking a cold bubble pays a full prompt-cache rewarm. The only sanctioned exceptions are the pre-existing `KeepAlive` and `inbox-drain`.
- Lock order `notifyMu → Policy.mu → registry.mu`; **never hold a lock across a PTY write, a launch, a `Kill`, a `Close`, or a `MemBytes()` probe.**
- `internal/sessions` stays a leaf (imports only `sync`, `internal/addr`, `internal/runner`).
- **Every suppression or silent-skip path increments a costmeter counter.** Silent suppression is a defect.
- Any newly registered supervisor check MUST be added to the inventory gate in `cmd/bubbles/checks_test.go`, which enumerates every check by name, interval and phase.
- **No existing test deleted or weakened.** If one fails, fix the change, not the test.
- `go build ./... && go vet ./... && go test -count=1 ./...` green at every task boundary, plus `-race` on touched packages.

---

## Task 1: `Introduce` must verify both bubbles exist

**Files:** Modify `internal/kernel/kernel.go` (`Introduce`), test alongside.

**The bug:** `IntroduceBy` (the bubble-facing path) validates both targets:
```go
if _, ok := k.Reg.Get(a); !ok { return fmt.Errorf("kernel: no bubble at %s", a) }
if _, ok := k.Reg.Get(b); !ok { return fmt.Errorf("kernel: no bubble at %s", b) }
```
`Introduce` — the **root** path — has no such check. That is why `introduce(0.9.34, 0.9.30.10)` returned `"introduced"` for an address that did not exist, minutes after it had vanished from every `contacts()` list.

- [ ] **Step 1: Write the failing test.** `Introduce` against a nonexistent address returns an error naming that address, and creates NO contact edge in either direction. Cover both argument positions — a phantom `a` and a phantom `b` — because a check on only one side is the same defect with a smaller blast radius.
- [ ] **Step 2:** Confirm it fails.
- [ ] **Step 3: Implement.** Mirror `IntroduceBy`'s existence checks. Use the identical error wording so the two paths are indistinguishable to a caller. Do NOT weaken `IntroduceBy`.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `fix(kernel): Introduce must verify both bubbles exist`

---

## Task 2: a returned spawn address must be durable

**Files:** Modify `internal/kernel/kernel.go` (`SpawnUnder`, a new `Persist` hook), `cmd/bubbles/app.go` (set the hook); tests alongside.

**The bug:** `SpawnUnder` adds to the **in-memory** registry and returns the address immediately. Persistence happens later and only on some paths: `cmd/bubbles/app.go:324` and `:346` call `saveFleet` after TUI-driven spawns, and `m.OnPersist` (`:311`) fires on a TUI version tick — but the IPC `spawn` handler (`:557-575`), which is what the `spawn()` MCP tool uses, **never persists at all**.

Confirmed on disk: `~/.bubbles/fleet.json` holds 33 bubbles and, under `0.9.30`, only `0.9.30`, `0.9.30.1`, `0.9.30.7`. The reported `0.9.30.9` and `0.9.30.10` are absent. That matches the report exactly: `0.9.30.10` existed in memory long enough to appear in `contacts()` and receive two messages, then was lost.

**Design:** the kernel cannot persist — `saveFleet` lives in `cmd/bubbles`. Add a `Persist func() error` hook to `Kernel`, set in `app.go` beside the existing `OnPersist` wiring, and have `SpawnUnder` call it **synchronously before returning**.

**The atomicity requirement — this is the point of the task:** if persistence fails, the spawn must fail. Remove the bubble from the registry, release the consumed spawn quota, and return the error. A returned address must mean "this bubble is durably recorded", or the caller is back to guessing — which is precisely what the reporter asked to have fixed.

- [ ] **Step 1: Write the failing tests.** Cover:
  - a successful spawn is durably persisted before the address is returned (assert the hook ran, and ran *before* the return)
  - **a spawn whose persist fails returns an error AND leaves no bubble behind** — no registry entry, no contact edges, and the spawn quota is not consumed
  - a nil `Persist` hook (tests, embedded use) behaves exactly as today — do not make the hook mandatory
  - the returned address, when persistence succeeded, is present in the registry
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Call the hook after `SeedBrain` and the contact grants, so a persisted fleet is internally consistent. **Do not hold any lock across the hook** — it does file I/O. Rollback must undo everything `SpawnUnder` did, in reverse order.
- [ ] **Step 4:** Tests pass; `-race` on `./internal/kernel/`.
- [ ] **Step 5: Commit** — `fix(kernel): a returned spawn address is durably persisted or the spawn fails`

---

## Task 3: `compact()` must actually compact

**Files:** Create `internal/kernel/compact.go` + test; modify `internal/kernel/kernel.go` (`Compact`), `cmd/bubbles/checks.go`, `cmd/bubbles/checks_test.go`, `internal/costmeter/costmeter.go`.

**The bug:** `kernel.go:968` types `/compact` + Enter into the PTY **immediately**, guarded only by `Alive()`. The caller is a bubble invoking the `compact()` MCP tool *from inside its own turn* — it is definitionally mid-turn and not accepting input, so the keystrokes are swallowed. Meanwhile `internal/mcpstdio/server.go:146` replies *"compaction scheduled — it runs after this turn; keep going, your context will shrink"*. **Nothing defers anything.** Reported: `0.9` made 7+ calls pinned at 792k; `0.9.34` made 7 calls and went 505k → 581k. Every subsequent turn then bills full context, including one-word replies.

**Why the obvious fix does not work — read before designing.** `deliverWhenReadyThen` waits on `s.InputReady()`, and `InputReady` is a **one-way latch**: `readyWatcher` only ever stores `true`, including on boot-deadline timeout and on process death. It means "was ready once", never "is ready now". It cannot detect the end of a turn. `SystemCompact` (`notify.go:154`) has the right *guards* but also writes inline — it only works because a background sweep calls it when the bubble is already idle.

**Design:** make the promise true by deferring.
- `Compact(a, focus)` records a **pending compact** for `a` and returns. It no longer writes.
- A new supervisor check flushes pending compacts for sessions whose turn has ended.
- **Turn-end signal:** `LastActivity()` (last *output*), the same signal `internal/health` uses. A session output-silent for the settle window has finished its turn. Do not use `InputReady`.
- Guards, all of which must hold before writing: session alive; not the focused bubble while `typingActive()`; output-silent for the settle window.
- Repeated `compact()` calls for one address collapse to a single pending entry (last focus wins) — a bubble that calls it 7 times must not type `/compact` 7 times.
- A pending compact that never becomes flushable expires after a bound, and **the expiry increments a costmeter counter** — a compact that silently never happens is the bug being fixed.

- [ ] **Step 1: Write the failing tests.** Cover:
  - `Compact` writes NOTHING at call time — this is the regression gate for the whole bug
  - the pending compact IS written once the session goes output-silent past the settle window
  - it is NOT written while the session is still producing output (turn in progress)
  - it is NOT written to the focused bubble while the operator is typing
  - seven `Compact` calls produce exactly ONE `/compact` write
  - a pending compact for a session that dies is dropped and never written
  - expiry increments the costmeter counter and drops the entry
  - the focus string is still sanitised through `compactCommand` (it is typed into a session; unsanitised it is a keystroke-injection path)
  - **`SystemCompact`'s behaviour is unchanged** — the Phase 2 context pump must keep working exactly as it does today
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Own mutex for the pending map, never held across a PTY write. Register the flush as a supervisor check and add it to the inventory gate. Update the `mcpstdio` reply only if its current wording is now inaccurate — with this fix, "it runs after this turn" becomes true.
- [ ] **Step 4:** Tests pass; `-race` on `./internal/kernel/ ./cmd/bubbles/`.
- [ ] **Step 5: Commit** — `fix(kernel): defer compact until the caller's turn ends, so it actually runs`

---

## Self-Review

**Coverage:** Bug 2a (introduce accepts phantoms) → Task 1. Bug 2b (spawn returns non-durable address) → Task 2. Bug 1 (compact never runs) → Task 3.

**Risk note:** Task 3 is the highest-value and highest-subtlety fix — it is the one costing real money, and the turn-end signal is a heuristic, not a guarantee. It is sequenced last so Tasks 1 and 2 land regardless. Task 2 changes a success path into one that can now fail; the rollback is the part most likely to be got wrong, and its test is the one that matters.
