# Learning Bubbles

A guided tour, from "what even is this" to "I can change the kernel safely".

Read it in order. Each section assumes the one before it and adds detail. If you only have five
minutes, read **Part 1**. If you're about to change code, you must read **Part 8 — The Laws**.

---

## Part 1 — The one-paragraph version

Bubbles is a terminal IDE for running a **fleet of Claude Code sessions** at once. Each session is
a "bubble": a real `claude` process in a real PTY, with its own working directory, its own
conversation, its own memory folder, and an address like `0.9.30.1`. Bubbles gives them an inbox
so they can message each other, a kernel that decides who is running and who is paged out, and a
dashboard so you can watch the whole thing. You are address `0` — the root, the human.

The hard problems it solves are not "how do I run several terminals". They are: **who is allowed
to talk to whom**, **who stays resident when memory is tight**, **how does a message reach a
process that isn't running**, and above all **how do you stop this costing a fortune**.

---

## Part 2 — Five ideas, and you can read the code

Everything in the repo is built from five nouns.

**Address** (`internal/addr`). A dotted path: `0` is root, `0.1` is root's first child, `0.1.2` is
that bubble's second child. Addresses are immutable strings compared with `==`, and they are
**never reused** — sequence numbers only move forward. That single property is load-bearing:
because an address can never be recycled, a stale reference can only ever fail to resolve, never
silently resolve to a *different* bubble.

**Bubble** (`internal/registry`). The durable record of one agent: address, name, directory,
model, goal, session id, webhook tokens, mute rules, contacts. A bubble exists whether or not it
is currently running. Spawning one costs no RAM at all — the process is created lazily on first
use.

**Session** (`internal/runner`, `internal/sessions`). The *running* half: a `claude` process in a
PTY. A bubble with no session is "cold"; with one, "hot". Sessions are created, killed and
recreated freely — the bubble survives.

**Message** (`internal/inbox`). Bubbles talk by filing messages in each other's inboxes. Messages
are durable, survive restarts, and — this is a rule, not a nicety — **are never dropped**.

**Kernel** (`internal/kernel`). The thing that owns all of the above and makes the decisions.

---

## Part 3 — What actually happens when you type `bubbles`

You run one binary, but three roles come out of it.

1. **The client** (`cmd/bubbles/client.go`) starts, looks for a **daemon** on a per-workspace unix
   socket, and starts one if it's missing. It also compares a *build stamp* (`daemon.build`) so a
   freshly installed binary can notice that the running daemon is stale.
2. **The daemon** (`cmd/bubbles/daemon.go`) is the long-lived process. It owns the kernel, the
   registry, every session, the IPC socket, the webhook server, and all the background sweeps. It
   survives you closing your terminal.
3. **The TUI** (`internal/tui`, Bubble Tea) is what you look at: the fleet tree on the left, your
   focused bubble's terminal in the middle, usage and health panels top-right.

When the kernel launches a bubble, `internal/runner` starts `claude` inside a PTY, in that
bubble's directory, inside its own cgroup scope (which is how bubbles can measure per-bubble RAM),
and writes two temp config files for it: an MCP config and a settings file, both named with the
daemon's pid and the bubble's address.

That MCP config is the interesting part.

---

## Part 4 — How a bubble talks to the fleet

A bubble is just Claude Code. It has no idea bubbles exists. So bubbles gives it **tools**.

Each launched `claude` gets an MCP server pointed at the bubbles binary itself, running in helper
mode. That helper (`internal/mcpstdio`) speaks JSON-RPC 2.0 over stdio — the MCP wire protocol —
and exposes `send`, `inbox`, `contacts`, `spawn`, `compact`, `schedule`, `mute`, `webhook` and the
rest. When a bubble calls `send(...)`, the helper relays it over a unix socket
(`internal/ipc` — newline-delimited JSON) to the daemon, which calls the kernel.

Two consequences worth internalising:

- **The helper forces identity.** It stamps `from` with its own address. A bubble cannot
  impersonate another bubble, because it never gets to choose who it is.
- **The socket path is per-workspace and stable**, not per-pid. It used to include the pid, which
  meant every daemon restart orphaned every live bubble's MCP bridge on a dead socket. A stable
  path lets the bridge reconnect.

---

## Part 5 — Follow one message all the way through

This is the single most useful trace in the codebase. `0.9` sends to `0.9.34`.

1. **Tool call.** `0.9`'s Claude calls the `send` MCP tool. The helper stamps `from: 0.9` and
   relays over IPC.
2. **Permission.** The kernel checks `internal/caps`: is `0.9.34` in `0.9`'s contacts? Bubbles do
   not have a global address book. You can only message someone you were introduced to. Root can
   introduce anyone; a bubble may only introduce two bubbles inside its own subtree.
3. **Filing.** The message is appended to the durable store (`internal/inbox`). **This always
   happens.** Everything after this point affects *notification only* — whether and how the
   recipient is told. Nothing downstream can lose the message.
4. **Policy.** `internal/notify` decides what to do. It is a pure package: no clock, no I/O, all
   time supplied by the caller. In order:
   - **Mute rules** — does this message match a predicate the recipient set (source, subject
     regex, body regex, with a TTL)? If so it is filed and *nothing else happens*. Crucially, mute
     gates the **wake**, not just the notice — see Part 7 for why that is the whole point.
   - **Coalescing** — several messages arriving together become one notice.
   - **Inline vs notice** — a small message is written straight into the recipient's terminal, so
     it costs no tool call and no extra turn. A large one becomes a "you have mail" nudge.
   - **The ceiling** — a hard token bucket, 6 notices per bubble per minute, that sits *below*
     policy and cannot be raised or disabled by any rule, capability or config.
5. **Delivery.** If the recipient is hot, the line is typed into its PTY. If the operator is
   mid-keystroke in that bubble, delivery is *held* and flushed when you pause. If the recipient is
   cold, an urgent message wakes it; a non-urgent one waits for the periodic drain.
6. **Recovery.** A sweep re-nudges bubbles that have unread mail they were never told about, so a
   lost notice can't leave an inbox silently growing.

If you understand that sequence you understand most of the kernel.

---

## Part 6 — The package map

**Pure policy** — no kernel import, no I/O, no `time.Now()`; all time is supplied by the caller so
tests are deterministic. These are the easiest packages to read and the safest to change:

| Package | Owns |
|---|---|
| `internal/addr` | Addresses and hierarchy |
| `internal/notify` | Mute rules, coalescing, inline-vs-notice, the flood ceiling |
| `internal/paging` | Which bubbles to evict, and in what order |
| `internal/transcript` | Reading context size out of a Claude Code transcript |
| `internal/health` | Stuck-bubble detection (reports only, never acts) |
| `internal/supervisor` | A named, panic-safe registry of periodic checks |

**State**:

| Package | Owns |
|---|---|
| `internal/registry` | The durable bubbles, guarded by one mutex |
| `internal/sessions` | The live session table — a *leaf* package (see below) |
| `internal/caps` | Contacts, spawn grants, permission |
| `internal/inbox` | Messages, read/muted state, the id sequence |
| `internal/tasks` | Harnessed tasks and verification |
| `internal/groups`, `internal/sched` | Group sessions, durable wake schedules |
| `internal/costmeter` | Every counter the fleet keeps about itself |

**Machinery**: `internal/kernel` (orchestration), `internal/runner` (PTY + process),
`internal/ipc` (socket protocol), `internal/mcpstdio` (the MCP bridge), `internal/logcap`
(size-capped rotating logs), `internal/bus`, `internal/tui`.

**Wiring**: `cmd/bubbles` — the entry point, the daemon, fleet persistence, the check registry,
webhooks, the context pump, headroom.

### Why `internal/sessions` is a "leaf"

It imports only `sync`, `internal/addr` and `internal/runner`. That is deliberate and enforced.
Because it *cannot reach* `Kill`, the registry or the notify policy, its mutex is structurally
incapable of being held across an unbounded operation. The lock discipline stops being a
convention someone has to remember and becomes a property of the import graph. **Do not add
imports to it.**

---

## Part 7 — The thing bubbles is really about: money

You cannot understand the design without the cost model.

Every turn, Claude Code re-sends the **entire conversation**. The API caches a matching prefix, so
a normal turn re-reads that prefix at about **0.1×** the input price. If the prefix doesn't match —
because the session restarted, or something earlier in the prompt changed — you pay full price for
the whole context. On a large conversation that is the difference between cents and **tens of
dollars for a single turn**.

Three consequences shaped the architecture:

**Paging is not free.** Killing a session to save RAM throws away nothing locally — but the next
use re-sends everything, and if the cache has expired you pay full freight. So `internal/paging`
scores each candidate by how *wasteful* evicting it would be — roughly its context size against how
long it has been idle — and evicts the least wasteful first. It also has a **grace floor**: a
bubble that was just woken has already paid its rewarm, so it goes to the back of the queue.
Without that floor, the 5-second budget sweep re-evicted the same freshly-woken bubble forever.

**But the budget is absolute.** Cost-awareness reorders *who* is evicted. It never reduces *how
many*. If every remaining candidate is expensive, the most expensive one still goes rather than
blowing the memory budget.

**Context growth is the dominant term.** Cost scales linearly with context size on every single
turn. So a pump watches each bubble's context (via `internal/transcript`) and nudges at 500k,
forcing compaction at 800k.

**And waking a cold bubble is the expensive event.** That is why mute gates the *wake* and not
merely the notice. A webhook pump firing every few minutes into a cold bubble was, in production,
paying a full uncached rewarm each time.

The `internal/costmeter` counters exist so all of this is *falsifiable*: evictions, rewarms,
notices suppressed, notices capped, inline deliveries, compactions accepted and dropped. A
mechanism you cannot measure is a mechanism you cannot trust.

---

## Part 8 — The Laws

These are invariants. They were each written after something broke. Violating one is a defect
even if the tests pass.

**1. No message is ever dropped.** `UnreadCount()` stays truthful. Mute and inlining affect
notification only. An inlined message is marked *muted*, never *read* — because `Take()` skips read
messages, so marking one read before a PTY write that can fail is silent data loss.

**2. The flood ceiling cannot be disabled.** 6 notices per bubble per minute, a token bucket
sitting below policy. It exists because one event was once re-emitted 100–178× across the fleet.

**3. Every suppression path increments a counter.** Silent suppression is a defect. If you add a
branch that decides not to do something, it must be countable — otherwise you get the
`compact()` bug, where a promise failed silently for weeks.

**4. Nothing on a background sweep may call `EnsureAlive`.** Waking a cold bubble costs a full
prompt-cache rewarm. There are exactly two sanctioned exceptions and you should not add a third.

**5. Never hold a lock across a PTY write, a launch, a `Kill`, a `Close`, or a `MemBytes()`
probe.** The shape is always: gather under the lock, do the slow thing outside it, mutate under it,
kill outside. Lock order is `notifyMu → Policy.mu → registry.mu`.

**6. Pure packages stay pure.** No `time.Now()`, no kernel import, no I/O. All time comes from the
caller. This is what makes the tests deterministic and fast.

**7. A returned value must mean what it says.** `spawn()` used to return an address for a bubble
that had never been durably saved; `introduce()` accepted addresses for bubbles that didn't exist;
`compact()` reported "scheduled" and did nothing. All three were the same bug wearing different
clothes.

**8. Watch out for logic races that `-race` cannot see.** Check-then-act split across two critical
sections is invisible to the race detector. This repo has shipped that bug twice: once as a
double-notification, once as a hung check permanently stranding every check that shared its tick.
Read, decide and mark belong in **one** critical section.

**9. `InputReady()` is a one-way latch.** It only ever becomes `true` — including on boot timeout
and on process death. It means "was ready once", never "is ready now". It is the obvious signal
for "can I type into this session" and it is a trap. Use output activity (`LastActivity()`)
instead.

---

## Part 9 — Health, and why it's shaped that way

Before hardening, a panic in any one of 17 background loops killed the whole daemon — there was no
`recover()` anywhere in the repo. Now every periodic job is a **named check** in
`internal/supervisor`, which recovers per check, records status, and runs each claimed check in its
own goroutine. That last detail matters: with a sequential batch, one check blocking forever left
every check behind it flagged as running *permanently*, silently removing them from the schedule.

Around that sit: crash-loop backoff so a bubble with a bad directory stops relaunching forever and
re-paying a boot context; conservative stuck detection that only ever *reports*; size-capped log
rotation using copy-truncate (a rename would have blackholed the daemon's stderr, because the file
descriptor is inherited by a child that outlives the process that opened it); and periodic reaps
for orphan verifiers and expired mute rules.

The TUI's health panel follows one rule that is easy to get wrong: **a metric whose source is
unavailable is omitted, never rendered as zero.** A zero reads as "verified healthy" when it
actually means "not measured".

---

## Part 10 — Changing bubbles without breaking it

**Build and test.** The Go toolchain may not be on your PATH:

```bash
PATH=$PATH:/home/rishi/goroot/bin go build ./... && go vet ./... && go test ./...
PATH=$PATH:/home/rishi/goroot/bin go test -race ./internal/kernel/
make install     # -> ~/.local/bin/bubbles
```

**A new binary does not take effect until the daemon restarts.** This catches everyone.

**Testing conventions.** `internal/runner` provides `FakeRunner` / `FakeSession` with knobs for
launch failure, lost resumes, input readiness and last activity. Time is injected, not slept on —
if you find yourself writing `time.Sleep` in a test, you are probably fighting the design.

**Make your tests discriminating.** The question is never "does this test pass" but "would it fail
if the behaviour were reverted?" Break your fix, run the test, watch it fail, restore. A test that
passes against the broken code proves nothing — and the `compact()` bug survived precisely because
the existing test asserted the *buggy* behaviour.

**Any new periodic check must be added to the inventory gate** in `cmd/bubbles/checks_test.go`,
which enumerates every check by name, interval and phase. A check that skips the gate defeats the
gate.

**Specs and plans** live in `docs/superpowers/`. Substantial work goes: spec → plan → task-by-task
implementation with a fresh review after each → one whole-branch review → merge.

---

## Part 11 — Where to look for…

| I want to… | Start at |
|---|---|
| understand addressing | `internal/addr/addr.go` |
| see what a bubble *is* | `internal/registry/registry.go` |
| trace a message | `kernel.Send` → `internal/notify/policy.go` → `kernel/notify.go` |
| see the tools a bubble has | `internal/mcpstdio/server.go` |
| understand eviction | `internal/paging/paging.go`, then `internal/kernel/paging.go` |
| see what runs on a timer | `cmd/bubbles/checks.go` |
| find how claude is launched | `internal/runner/local.go` |
| know what the fleet measures | `internal/costmeter/costmeter.go` |
| understand persistence | `cmd/bubbles/fleet.go` |
| read the dashboard code | `internal/tui/view.go` |

---

## Part 12 — The mental model, one more time

A **bubble** is a durable record; a **session** is its optional running process. The **kernel**
owns who exists, who is running, who may talk to whom, and who gets evicted. **Pure policy
packages** make the decisions; the kernel applies them; `cmd/bubbles` wires it to a terminal, a
socket and a disk.

Nearly every non-obvious design choice in this repository traces back to one of two sentences:

> *Waking a cold bubble costs a full prompt-cache rewarm.*

> *A promise the code does not keep is worse than no promise at all.*

Welcome aboard.
