# Spec — Harnessed Task Assignment & Verification

**Status:** IMPLEMENTED (see §13 for as-built deltas) · **Author:** design session (Rishi + Claude) · **Grounded against:** working tree at `internal/kernel/kernel.go`, `internal/groups/groups.go`, `internal/runner/local.go`, `internal/mcpstdio/*`

## 1. What we're building

Two capabilities, layered on the existing kernel without disturbing the fast `send()` path:

1. **`assign_task()`** — a bubble higher in the hierarchy hands a scoped task, *with acceptance criteria attached*, to a lower bubble.
2. **A per-task `taskBubble`** — an ephemeral, independent verifier spawned by the kernel when a task is assigned. It writes/runs tests and/or checks the checklist, and it is the **only** path a completion can take back to the assigner.

The design goal is that **an agent cannot report unverified work as done** — not by convention, but because the kernel enforces the completion route. `send()` stays dumb and fast; only assigned tasks are gated.

## 2. Roles & terminology (fixed)

| Term | Meaning |
|---|---|
| **assigner** | bubble up the tree that calls `assign_task()` |
| **worker** | the assignee that does the task |
| **taskBubble** | ephemeral verifier spawned by the kernel for one task; lives under a per-task group |
| **contract** | the acceptance criteria carried by the assignment: test files that must pass and/or a checklist |

## 3. The one load-bearing primitive: the enforced route

Everything else reuses machinery Bubbles already has (spawn, groups, PTY-notify, caps). The single genuinely new kernel guarantee is:

> A task's completion travels **worker → taskBubble → assigner**, and the worker has no way around it.

Without this, a worker can `send(assigner, "done")` directly and the verifier is theater. With it, "done" is a *state transition the kernel owns*, not a message a worker emits.

Concretely: while a task is open, the worker does **not** get a "done" channel to the assigner. It gets a kernel-mediated `submit_task(task_id, ...)` that routes to the task's `taskBubble`. The assigner only ever receives the completion notice the kernel types into its window *after* the taskBubble approves (§7).

## 4. Data model

A `Task` record (new store, sibling to `inbox`/`sched` — keep it in a real store, **not** a markdown file concurrent bubbles write; this respects the harness doc's "don't let the folder be the database" rule):

```
Task {
  ID          string
  Assigner    addr.Address
  Worker      addr.Address
  TaskBubble  addr.Address   // the verifier
  Group       string         // per-task group container
  Brief       string         // the charter given to the worker
  Contract    Contract
  State       enum           // see §6
  Rounds      int            // reject→resubmit count
}

Contract {
  TestFiles []string   // paths (in worker's dir) that must pass — computational sensor
  Checklist []string   // items a verifier must confirm — inferential sensor
  Mode      enum        // AUTO (kernel runs tests inline) | BUBBLE (spawn taskBubble)
}
```

`Mode` lets trivial deterministic-only tasks skip the bubble (§9 cost note).

## 5. New capability: the assignment graph

`assign_task` authority is a **separate graph from the spawn tree** (`internal/caps`). Reasons, decided in this session:

- Spawn tree = ownership/lifecycle (who can `delete`/`edit` whom). Stable.
- Assignment graph = who may direct whom. **Live-editable** — the operator can re-parent or grant/revoke an assign-edge at runtime without respawning.

Add to `caps`: `CanAssign(from, to) bool`, `GrantAssign(from, to)`, `RevokeAssign(from, to)`. Default seeding: a bubble may assign to its spawn-descendants (mirrors today's contact wiring in `SpawnUnder`, kernel.go:961-963), but the edge is thereafter mutable independent of the tree.

The static `fleet.yaml` idea from the handoff doc (R3) becomes a **snapshot** of the live assignment graph, not the source of truth.

## 6. Task state machine

```
ASSIGNED ──(worker starts)──▶ IN_PROGRESS
IN_PROGRESS ──(submit_task)──▶ SUBMITTED
SUBMITTED ──(taskBubble verifies)──▶ VERIFYING
VERIFYING ──approve──▶ APPROVED ──▶ (completion typed into assigner window) ──▶ CLOSED
VERIFYING ──reject──▶ IN_PROGRESS   (feedback typed into worker window; Rounds++)
any ──(assigner cancels)──▶ CANCELLED
```

The taskBubble **persists across reject→fix→resubmit rounds** — it remembers what it asked for last round, so its feedback compounds ("still failing `TestBar`; checklist item 3 still unchecked"). It is deleted only on `CLOSED`/`CANCELLED` (its group is torn down with it — `Store.PurgeMember`, groups.go).

## 7. Lifecycle & code seams

**On `assign_task(worker, brief, contract)`** (new handler in `internal/mcpstdio/server.go` switch ~L112, tool def in `tools.go`):
1. `caps.CanAssign(assigner, worker)` — reject otherwise.
2. Create `Task` (state `ASSIGNED`).
3. Kernel spawns the taskBubble via `SpawnUnder(by=root/kernel, parent=taskGroup, …)` (kernel.go:947). **Spawned by the kernel, never the worker** — this is what makes the verdict independent. Give it the contract as its charter/brain.
4. `CreateGroup` for the task (groups.go); add worker + taskBubble; optionally `SetSession` the taskBubble as the group's coordinator (mirrors existing coordinator field).
5. Type the brief into the worker's window (existing `deliverMessage` notify path, kernel.go:448 → `s.Write`).

**On `submit_task(task_id, summary|diff)`** (new tool, worker-only while task open):
1. Kernel routes the submission to the task's **taskBubble** (not the assigner). State → `SUBMITTED`.
2. taskBubble runs the contract:
   - `TestFiles`: run in the worker's dir via the same `runner` seam that launches claude; capture pass/fail.
   - `Checklist`: taskBubble (an agent) judges each item, optionally hiring its own subagent.
3. taskBubble returns a **structured verdict** (pass/fail + per-item + fix hints) through a kernel-mediated `verdict(task_id, …)`. State → `VERIFYING`.

**On verdict:**
- **approve** → kernel forwards the *same* submission up to the **assigner** and types the completion notice into the assigner's window (the PTY-notify path, kernel.go:491). State → `APPROVED`/`CLOSED`. taskBubble + group deleted.
- **reject** → kernel types **LLM-optimized feedback** (the taskBubble's specific fixes — "how to fix", not just "failed") into the **worker's** window. State → `IN_PROGRESS`, `Rounds++`. taskBubble persists.

## 8. Folder / brain model (companion change, from same session)

Independent of tasks but part of the same evolution:

- **Group-level workspace** — the flat/ICM conventions (map `CLAUDE.md`, specs, skills) live at the group's shared working folder. All bubbles in the group inherit it by `cd`-ing there (today's `dir` arg to `Launch`, local.go:163/236). Read-mostly, shared — two workers in one folder is fine.
- **Per-bubble brain** — keyed by **address** (e.g. `.bubbles/brains/<addr>/`), never shared. Answers the "two bubbles, one folder → two brains?" question: **yes, two brains, and that's correct.** Brain follows the bubble; workspace is shared ground.
- **Kernel seeds the skeleton at spawn** — provisioning the folder layout is a deterministic kernel operation (extend `SpawnUnder`), not an agent action. Note for David: this means the kernel *creates/maintains the folder structure*, it does not edit the *contents* of `CLAUDE.md`.
- Any fleet-shared ledger (decisions / recurring-failures) goes through a kernel-serialized `log_decision` tool into the store — **not** a markdown file multiple bubbles write concurrently.

## 9. Cost / model notes (verified against code)

- **Bubble-per-task is heavy** if the only criterion is "tests pass" (a whole claude session to run one command). Hence `Contract.Mode = AUTO` — kernel runs the deterministic tests inline and only spawns a taskBubble when there's a judgment call (`Checklist` present or `Mode = BUBBLE`). Decide default per pilot.
- **Model split is real but currently binary.** `internal/runner/local.go:201-204`: in subscription mode any `opts.Model != "fable"` collapses to `"opus"`. So "each bubble its own model" today means **fable-or-opus only** — haiku/sonnet aren't reachable unless on Bedrock (`ANTHROPIC_MODEL`). If we want coordinator=fable / worker=opus / verifier=cheap-tier, this mapping needs widening. Flag for David: the Fable/Opus split works today; a *three-tier* split does not, yet.

## 10. MCP surface (new tools)

| Tool | Caller | Effect |
|---|---|---|
| `assign_task(to, brief, test_files?, checklist?)` | assigner (gated by `CanAssign`) | opens a task, spawns taskBubble |
| `submit_task(task_id, summary?, diff?)` | worker | routes submission to its taskBubble |
| `verdict(task_id, pass, notes)` | taskBubble | approve→assigner / reject→worker |
| `log_decision(text)` | any | serialized append to shared ledger |
| `tasks()` | assigner/worker | list one's open tasks + states |

Registration mirrors existing tools: entry in the slice in `internal/mcpstdio/tools.go`, dispatch case in `internal/mcpstdio/server.go` (~L112), kernel method behind each.

## 11. Build order

1. `Task` store + state machine + `caps` assignment graph (no UI yet).
2. `assign_task` + `submit_task` + the **enforced route** (§3) — the killer primitive. Kernel-run `AUTO` verification only.
3. `taskBubble` spawn + `verdict` for `BUBBLE`/checklist mode (inferential half).
4. Brain/workspace seeding at spawn (§8) + `log_decision`.
5. Live assignment-graph editing in the TUI (re-parent, grant/revoke assign edges).

MVP = steps 1–2: turns "autonomous fleet" into "harnessed fleet" touching only the send/spawn/caps seams.

## 12. Open decisions (for David / Rishi)

1. **AUTO vs BUBBLE default** — spawn a verifier for every task, or only when a checklist is present?
2. **Submission input** — diff, worker-written summary, or both? (Diff is the most objective sensor input.)
3. **taskBubble reuse** — strictly per-task, or one persistent verifier per group holding all its contracts? (Spec assumes per-task/ephemeral for isolation.)
4. **Three-tier models** — widen the local.go model mapping now, or stay fable/opus binary?
5. **Pilot workspace** — point a fleet at `reva` (coding) or a smaller single-purpose one to shake out contract/config formats first?

## 13. As built (deltas from the draft above)

Implemented in `internal/tasks/`, `internal/kernel/tasks.go`, `internal/kernel/harness.go`, plus MCP/IPC wiring. Where the implementation deviates from the draft:

- **Enforced route = annotation, not blocking.** `Send` is never blocked; while a task is open, a worker→assigner subject is prefixed `[task tN open — unverified]`, and the only completion notice is the kernel-composed `✅ task tN verified & complete`. One map scan on the send path.
- **No Mode enum.** The contract is `check_cmd` (deterministic, kernel-run via `sh -c`, 10-min timeout, output tail returned) and/or `checklist` (spawns a verifier). Check always runs first; the verifier only sees submissions that already pass it. An empty contract is rejected — an ungated task is just a message.
- **Deterministic check runs SYNCHRONOUSLY inside `submit_task`** and its result is the tool output: a failing check bounces to the worker in the same tool call, with the failure output — no message round-trip, immediate LLM-optimized feedback.
- **Assignment authority = spawn-descendant rule** (root → anyone). No separate editable assign-graph yet; `assign_task` is advertised only to spawn-granted bubbles. Live re-parenting / grant edges remain future work.
- **Verifier lifecycle:** kernel-spawned via `Reg.Add` (no spawn budget consumed, worker has no authority over it), named `verify:tN`, launched lazily in the worker's dir, persists across reject→resubmit rounds, reaped ~60s after final verdict (so its `verdict` tool call completes). Deleting a worker/assigner cancels its open tasks; deleting a verifier degrades the task to deterministic-only.
- **State machine simplified:** `open → checking → done | cancelled` (reject returns to `open`, rounds++). No separate SUBMITTED/VERIFYING/APPROVED states — `checking` covers the in-flight window.
- **Brains:** kernel seeds `<workspace>/.bubbles/brains/<addr>/BRAIN.md` at spawn (`SeedBrain`); the bubble is told the path in its system prompt. Keyed by address — shared workspaces, separate brains.
- **Ledger:** `log_decision` appends via kernel-serialized `LogDecision` to `.bubbles/memory/decisions.md`.
- **Persistence:** `.bubbles/tasks.json` (2s version-polled saver + boot load), matching the inbox/schedules pattern.
- **Security piggyback:** control characters are stripped from names/subjects before they are typed into a recipient's PTY (`sanitizePTY`) — closes the cross-bubble prompt-injection channel for the typed path.
- **Known limits:** a worker can still send a fake "✅…" via send() for a task id that is already closed (assigners should trust `tasks()`, which is authoritative); the IPC socket still trusts caller-asserted `From` (R0 item two — future work); three-tier model routing still collapses to fable/opus in subscription mode.
