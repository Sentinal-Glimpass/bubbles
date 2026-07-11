# Bubbles — Capability Additions (2026-07-11)

**Scope:** everything shipped today, from kernel-enforced task assignment through the programmatic control webhook. Four features, four distinct commits on `main`, all tests + `go vet` green (one pre-existing PTY-timing flake noted).

| # | Feature | Commit |
|---|---|---|
| 1 | Kernel-enforced task verification (harnessed fleet) | `a3d8a13` |
| 2 | Responsive usage panel + TASKS/DEACTIVATED sections | `7375957` |
| 3 | Pinned dive footer (which bubble you're in) | `77371a9` |
| 4 | Control webhook — programmatic spawn/delete | `e001d43` |

The through-line: **move quality and control from prose that agents *may* obey into kernel mechanisms the runtime *enforces*.** Bubbles' kernel already mediates every message and spawn, so it's the natural place to make the harness load-bearing.

---

## 1. Kernel-enforced task verification (`a3d8a13`)

**The problem.** Before today the only quality mechanism in the fleet was hoping agents check each other's work. A worker could report "done" and nothing verified it.

**What shipped.** A task now travels an enforced route the kernel owns: **worker → deterministic check → (independent verifier) → assigner**. A completion the assigner sees is always a *verified* one.

New MCP tools:
- `assign_task(to, brief, check_cmd?, checklist?)` — a spawn-granted bubble assigns contract-gated work into its subtree (root → anyone). The contract is a shell `check_cmd` that must exit 0 in the worker's dir, and/or a `checklist`.
- `submit_task(task_id, summary)` — the **only** way to complete a task. The kernel runs the check **synchronously inside the tool call**; a failing check bounces straight back to the worker *with the output*, and nothing reaches the assigner.
- `verdict(task_id, pass, notes)` — an independent verifier bubble's ruling.
- `tasks()` — authoritative task state (trust over any message claim).

**Design decisions worth knowing:**
- **Enforced route = annotation, not blocking.** `send()` stays fast and is never blocked; while a task is open, a worker→assigner subject is auto-prefixed `[task tN open — unverified]`, and only the kernel composes the `✅ task tN verified & complete` notice. So a worker literally cannot hand off an unverified "done."
- **The verifier is an independent bubble.** For checklist tasks the kernel spawns a `verify:tN` bubble (no spawn budget consumed, worker has zero authority over it — so it can't mark its own homework). It launches lazily in the worker's dir, persists across reject→resubmit rounds (its feedback compounds), and is auto-reaped ~60s after its final verdict.
- **Deterministic-only tasks skip the bubble** — no agent spun up just to run `go test`.
- State machine: `open → checking → done | cancelled` (reject returns to `open`, rounds++). Persisted to `.bubbles/tasks.json` so open routes survive a restart.

**Companion additions (same commit):**
- **Per-bubble brain folders** — every spawn seeds `.bubbles/brains/<addr>/BRAIN.md`, keyed by *address*. Two bubbles sharing a workspace get two brains; the bubble is told its brain path in its system prompt.
- **`log_decision(text)`** — appends to a kernel-serialized shared ledger (`.bubbles/memory/decisions.md`), so concurrent bubbles never corrupt it.
- **Security:** control characters are now stripped from names/subjects before they're typed into a recipient's PTY (`sanitizePTY`) — closes a cross-bubble prompt-injection channel.

**Code seams:** `internal/tasks/` (store), `internal/kernel/tasks.go` (verbs + route), `internal/kernel/harness.go` (brains + ledger), MCP/IPC wiring. Full spec: `docs/task-verification-spec.md` (§13 = as-built).

---

## 2. TUI: responsive panel + dedicated sections (`7375957`)

Three fleet-view fixes:
- **Resize no longer jams the usage panel into the tree.** `overlayTopRight` now measures fit and **stacks the panel above the tree** when the terminal is too narrow for a ≥3-column gap, instead of rendering it inline with the bubble rows.
- **Deactivated agents** leave the main tree and collect in a bottom **DEACTIVATED** section (hidden from root's view; `x` re-activates).
- **Task verifier bubbles** are hidden from the tree and shown in a **TASKS** section that empties itself when the kernel reaps them on completion.
- Section dividers are non-selectable; cursor navigation skips them.

**Code seams:** `internal/tui/model.go` (`fleetRows`, `buildVisibleRows`, `step`), `internal/tui/view.go` (`overlayTopRight`, section rendering).

---

## 3. Pinned dive footer (`77371a9`)

When you dive into a bubble, a reverse-video footer is pinned to the bottom row showing `● <address> <name> — Ctrl-\ Ctrl-\ back to fleet`, so you always know which bubble you're in.

**How it's safe:** the bubble's PTY is sized one row short of the terminal and a scroll region protects the reserved row, so claude never draws over or scrolls away the footer. All terminal writes (claude output + footer repaints) share one mutex, so they never interleave mid-escape-sequence. Skipped on terminals under 3 rows.

**Code seams:** `cmd/bubbles/app.go` (`diveStatus` type + `diveInto` sizing/attach). *Caveat: the interactive path isn't auto-testable without a live claude; unit tests cover the footer painter only.*

---

## 4. Control webhook — programmatic spawn/delete (`e001d43`)

**The ask:** a programmatic way for a spawn-capable bubble to spawn/delete agents without an agent in the loop.

**What shipped.** `control_webhook` (tool, spawn-granted bubbles only) mints a `/c/<token>` endpoint that runs fleet actions **as that bubble**:

```
POST /c/<token> {"action":"spawn","name":..,"description":..,"dir":..,"model":..}  → {ok,addr,webhook}
POST /c/<token> {"action":"delete","target":"0.3.1"}                               → {ok,removed}
POST /c/<token> {"action":"list"}                                                  → {ok,children}
```

**Design decisions:**
- **The token IS the capability** — no new auth. Actions execute with the owning bubble's existing spawn/manage caps (delete only within its subtree). A non-spawn bubble is refused a URL entirely.
- **Separate from the message webhook** (`/w/`). A shared notification URL never carries control authority; control tokens are minted only where spawn is granted.
- **Control responses return addresses** (unlike the anonymous `/w/`) — the token is an authenticated management secret. Spawn returns the child's address *and* its message webhook, so a script can chain: spawn a worker → grab its webhook → POST it work.
- Token persists across restarts and is rotatable (`rotate=true`) to revoke a leaked URL. Permission/cross-subtree errors collapse to a clean 403.
- This is the **LLM-as-Code** path: the kernel performs spawn/delete deterministically; the bubble needn't be awake or reason.

**Code seams:** `cmd/bubbles/webhook.go` (`/c/` handler), `internal/kernel/kernel.go` (`ControlWebhookURL`/`Rotate`/`ResolveControlToken`), `internal/registry` (control token), MCP/IPC wiring.

---

## Cross-cutting themes

1. **The kernel is the enforcement point.** Every feature leans on the fact that the kernel mediates messages, spawns, and now tasks — so guarantees are properties of the runtime, not agent cooperation.
2. **Capabilities gate everything.** Task assignment, verifier independence, and control webhooks all ride the existing spawn/subtree capability model — no parallel auth.
3. **Each feature is one focused commit** — independently reviewable and revertible.

## Known limits / deferred

- Live re-parenting / editable assignment graph (assignment authority currently follows the spawn tree + root-anywhere).
- IPC socket still trusts caller-asserted `From` (identity spoofable by a local process) — flagged for a future hardening pass.
- Three-tier model routing: subscription mode still collapses any non-`fable` model to `opus` (`internal/runner/local.go`), so a cheap third tier isn't reachable there.
- `TestDaemonRelay` is a pre-existing PTY-timing flake (fails the same way on HEAD under package-level load; passes in isolation).

## Build / run

Go toolchain is at `~/goroot` on this box: `PATH=$PATH:~/goroot/bin make build`, then restart the daemon to pick up the new binary.
