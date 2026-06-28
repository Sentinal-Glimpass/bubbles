# Bubbles — an agent-native terminal IDE

**Status:** Design / MVP 1
**Date:** 2026-06-28
**Working title:** Bubbles (final name TBD)

---

## 1. Vision

Traditional IDEs are built around *one human typing into files*. Bubbles is built
around *many agents working in parallel*. Each agent is a real Claude Code session
rendered as a **bubble** in a sleek, zoomable terminal tree. The human's job shifts
from *writing code* to *supervising a fleet of agents*: spawn them, watch them ping
you when they need you, dive into any one to collaborate, pop back out.

It is a **mission-control dashboard for agents**, living entirely in the terminal.

## 2. The kernel (the one idea everything is built from)

The whole system bottoms out in **one atom and one verb**, the way CPython bottoms
out in a handful of core objects or Lisp bottoms out in `eval`/`apply`.

**Atom — a Bubble.** Three things:
- an **address** (a mailbox you can reach it at)
- a **session** (a real `claude` process — or, at the root, the human)
- the ability to **`send`** a message to an address

**Verb — `send`.** Every feature is `send` recomposed, so the system grows on itself
without inventing new mechanisms:

| Feature | Definition in the kernel |
|---|---|
| Spawn a bubble | `send` to the spawner → returns a new address |
| Persona | a bubble whose session carries a system prompt |
| Bubble pings you | the bubble does `send(root, msg)` → renders as a blink |
| You dive in | you `send` to a bubble; its replies stream back |
| Loop / goal | **native Claude Code features** (`/loop`, goal-setting) — not reimplemented |

The architectural payoff is **closure + self-hosting**: there is no orchestrator
*outside* the system. The dashboard is the root bubble. The UI is a view of the
message graph.

## 3. The one-tree principle

Address, filesystem, and UI are **the same tree seen three ways**:

```
address      0.1.2
folder       <workspace>/0.1/0.1.2/
UI           click bubble 0 → see 0.1 → click → see 0.1.2
```

- **The address tree (spawn lineage) is canonical** (`0` = you/root). It is always
  a tree.
- **Folder is a mapping on top of the address tree, not the tree itself.** The
  default is *one folder per bubble* — load-bearing, not cosmetic: parallel `claude`
  sessions editing shared files would clobber each other, so a directory per bubble
  makes the fleet safe to run by default. But a folder **may hold multiple bubbles**
  when you deliberately want them collaborating on the same files (you accept the
  clobber risk on purpose). (When bubbles must share one repo safely, the folder is
  backed by a **git worktree** — same convention, later.)
- **The UI is a zoomable tree** that mirrors the address structure.

## 4. Capabilities & inter-agent communication

Capabilities are explicit and gate how far autonomy spreads. Authority flows from
root: nothing happens that root did not enable.

**Spawn.**
- **`spawn` is a capability granted only by root (you).** A bubble cannot spawn
  unless you have personally blessed it. This dissolves the v1/v2 tension: the
  system is a full tree (recursion allowed at any depth), but nothing spawns
  autonomously until you loosen your grip. **v1 and "autonomous fleets" are the same
  code, different generosity.**
- A grant may carry a **budget** (e.g. "≤3 children, depth ≤1") to prevent
  fork-bombs. The budget is itself a capability.

**Inter-agent communication — the "SMS + contacts" model (in MVP 1).**
Inter-agent comms is crucial, so it ships in v1 — in its absolute minimal form.
SMS, not Discord.
- Every bubble has an **address** (its phone number) and an **inbox**. Messages are
  *injected on arrival* (the interrupt mechanic, §5) — no polling, no `inbox()` tool.
- One verb: **`send(to, message)`**. A bubble may only text addresses in its
  **contacts**.
- A fresh bubble's contacts contain **only root**. So it can always report to you,
  and knows no peers — independent by default.
- **Root introduces** two bubbles to make them peers: **`introduce(A, B)`** drops
  each one's address into the other's contacts. `introduce` is itself just root
  texting each bubble a contact — not a new mechanism. Peer comms therefore exists
  **only where root made an introduction.**
- Worker tools: **`send`, `contacts`**. Root also holds **`spawn`, `introduce`**.
- **Channels are deferred to v2** as pure sugar: a channel is just "introduce
  everyone to everyone, plus one broadcast address," and composes from the same
  `send` + `introduce` primitives. Building SMS first costs nothing and keeps v1 tiny.

## 5. Message delivery

Chosen model: **interrupt (deliver now)**. A message lands immediately rather than
waiting politely for a turn boundary — this is what makes the fleet feel *alive*.

- worker → root: surfaces instantly as a **blinking node** with a short subject in a
  text box. Click to dive in and read the full thread.
- root → worker: breaks into the worker's current turn (tap on the shoulder).

> **This is the single riskiest mechanic** (interrupting a live, turn-based `claude`
> session via its PTY). It is de-risked by a spike *before* anything else is built
> (see §10 Milestones). If reliable PTY interrupt proves infeasible, the documented
> fallback is **turn-boundary delivery** via a Claude Code `Stop` hook that drains
> the bubble's inbox at the end of each turn.

## 6. Workspaces and runners (local now, SSH designed-in)

A **Workspace** is a root context with a `host` setting:

- `host: local` — bubbles launch on this Mac.
- `host: ssh://user@vm` — bubbles launch on a remote VM over SSH.

The IDE is a **thin control plane**: your Mac is mission control; the heavy agent
work can run on whatever VM you point a workspace at. This drops in behind one
interface and changes nothing else in the kernel:

```
type Runner interface {
    Launch(addr Address, dir string, opts SpawnOpts) (Session, error)
    Attach(addr Address) (PTY, error)   // for dive-in
    Kill(addr Address) error
}
```

`LocalRunner` ships in MVP 1. `SSHRunner` is the same interface, built second.

## 7. The bubble standard library

Every spawned session imports one small skill — **`bubble-citizen`** — that makes an
ordinary `claude` process fleet-aware: how to use `send`, how to report to root,
conventions for subjects/pings. This is the most Python part of the design: every
bubble *imports the same fundamental skill*. New fleet capabilities later = new
skills in this stdlib. The skill is delivered into each bubble's folder/config at
launch, alongside an MCP server exposing the `send` (and root-only `spawn`) tools.

## 8. MVP 1 scope

**In:**
- Sleek zoomable TUI tree of bubbles (Go).
- Spawn worker bubbles from root; each gets an address + its own folder + a real
  `claude` session via `LocalRunner`.
- `send` bus + MCP server exposing tools to bubbles: `send`, `contacts` (and root's
  `spawn`, `introduce`).
- **SMS inter-agent comms:** root-seeded contacts; `introduce(A, B)` makes two
  bubbles peers; bubbles `send` directly to contacts. Bubble → root pings render as
  blinking nodes with a subject text box.
- Dive into any bubble (full-screen takeover of its PTY); `esc` back to the tree.
- Interrupt-style delivery (with documented Stop-hook fallback).
- Root-only `spawn` grant with budget (mechanism present even if rarely used in v1).
- Folders default to one-per-bubble; sharing a folder is allowed.

**Out (v2 — "coming soon"):**
- Autonomous/perpetual spawn (loosening the grant), channels (Discord-style invites
  / broadcast), SSH runner, git-worktree-backed shared folders.

## 9. Architecture & components

Designed as small, independently testable units that talk through narrow interfaces.

| Unit | Responsibility | Depends on |
|---|---|---|
| `addr` | hierarchical addresses, parse/format, lineage | — |
| `bus` | in-memory message routing, mailboxes, subscribe | `addr` |
| `registry` | bubble lifecycle + state (idle/working/waiting/done), tree | `addr` |
| `caps` | capability tokens (spawn, budget) + per-bubble contacts, grant/check | `addr` |
| `runner` | `Runner` interface + `LocalRunner` (PTY, process mgmt) | — |
| `mcp` | MCP server exposing `send`/`contacts`/`spawn`/`introduce` to a session | `bus`, `caps` |
| `tui` | Bubbletea/Lipgloss zoomable tree, blink, dive-in | `bus`, `registry` |
| `app` | wires everything; workspace config | all |

**The kernel never imports `claude` directly** — it only ever talks to `Runner`.
This single rule is what makes ~90% of the system testable with fakes.

**Tech stack:** Go, Bubbletea + Lipgloss (TUI), Model Context Protocol for the
`send`/`spawn` tools, real `claude` CLI as the session. Ships as a single binary.

## 10. Testing strategy

Testability is a first-class design constraint, achieved by the `Runner` boundary
and the pure-Go kernel.

**1. Spike first (de-risk the PTY).** Before building features, a throwaway spike
proves we can: launch a real `claude` in a PTY, attach/detach, inject input, and
**interrupt a running turn**. This validates §5. The MVP architecture is only
committed once this is known to work (or the Stop-hook fallback is adopted).

**2. Unit tests — the kernel, no `claude` needed.** `addr`, `bus`, `registry`,
`caps` are pure Go and fully unit-tested (addressing math, routing, mailbox
delivery, state transitions, capability grant/budget enforcement).

**3. `FakeRunner` — deterministic end-to-end.** A fake implementation of `Runner`
launches a **scriptable fake session** (a tiny program that emits known output and
can call the bus) instead of real `claude`. This tests spawn → send → ping → blink →
dive-in **end-to-end, deterministically, with zero tokens and no network.** This is
the workhorse of the suite.

**4. MCP tool tests.** The `send`/`contacts`/`spawn`/`introduce` tools are tested
directly against the bus and `caps`, including: a fresh bubble's contacts are exactly
`{root}`; `send` to a non-contact is denied; `introduce(A, B)` then enables mutual
`send`; a worker without the grant is denied `spawn`; budget exhaustion is enforced.

**5. TUI tests.** Bubbletea's `teatest` drives the UI: feed bus events, assert the
tree renders, a ping makes the right node blink with the right subject, `enter`
dives in, `esc` returns.

**6. Opt-in integration smoke test (real `claude`).** Behind a build tag / env flag
(slow, costs tokens, not in default CI): spawn one real bubble, have it do a trivial
task, emit a ping, confirm it blinks and is reachable. One happy path, run manually
or in a nightly.

**7. SSH (v2).** `SSHRunner` tested against a local `sshd` (Docker/localhost) reusing
the same `Runner` contract tests as `LocalRunner`.

## 11. Open questions / risks

- **PTY interrupt reliability (§5)** — the spike settles this. Highest risk.
- **Receiving async messages inside a live `claude`** — interrupt injection vs Stop
  hook; spike informs the choice.
- **MCP server-per-bubble vs one shared server** — start with one shared bus server
  addressed by bubble; revisit if isolation needed.
- **Final project name.**

## 12. Milestones

1. **Spike:** PTY launch/attach/interrupt of a real `claude` (de-risk §5).
2. **Kernel:** `addr` + `bus` + `registry` + `caps`, fully unit-tested.
3. **FakeRunner + MCP `send`:** end-to-end spawn/ping with fakes, tested.
4. **TUI:** zoomable tree, blink, dive-in, wired to bus/registry (`teatest`).
5. **LocalRunner + real `claude`:** opt-in integration smoke test.
6. **Polish:** workspace config, single-binary build, README for OSS launch.
