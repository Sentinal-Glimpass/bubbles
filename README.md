# Bubbles

> An agent-native terminal IDE — mission control for a fleet of Claude Code agents.

Traditional IDEs are built around *one human typing into files*. **Bubbles** is built
around *many agents working in parallel*. Each agent is a real [Claude Code](https://claude.com/claude-code)
session rendered as a **bubble** in a zoomable terminal tree. Your job shifts from
*writing code* to *running a fleet*: spawn agents, watch them ping you when they need
you, dive into any one to collaborate, and let them message each other — all from the
terminal, no Electron, a single native binary.

```
BUBBLES — fleet   permissions: ALLOW-ALL (skip permissions) (ctrl+p)

> ▾ ● 0 root (4)
    ▸ ● 0.1 api (2) [1] ✉2  ✉ "auth bug — need a decision"
      ○ 0.2 docs
    ▾ ● 0.3 tests (1)
        ◐ 0.3.1 e2e
  ↑/↓ move · →/← expand/collapse · enter dive · 0-9 jump · m+0-9 set slot · n new · i introduce · q quit
```

## Requirements

- **[Claude Code](https://claude.com/claude-code)** — the `claude` CLI must be
  installed and authenticated (run `claude` once and sign in). Bubbles launches real
  `claude` sessions, so this is required.
- **Go 1.24+** — to install/build the binary (`go version` to check).

## Install

```bash
go install github.com/Sentinal-Glimpass/bubbles/cmd/bubbles@latest
```

This puts `bubbles` in `$(go env GOPATH)/bin` (usually `~/go/bin`). Make sure that's
on your `PATH`:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"   # add to ~/.zshrc or ~/.bashrc to persist
```

<details>
<summary>Or build from source</summary>

```bash
git clone https://github.com/Sentinal-Glimpass/bubbles.git
cd bubbles
make install        # builds to ~/.local/bin/bubbles
# or: make bin      # builds ./bin/bubbles
```
</details>

## Quick start

From any project directory:

```bash
cd ~/my-project
bubbles
```

- Press **`n`** to spawn a bubble: type a persona (e.g. `api`), pick a folder, done.
- Press **`Enter`** on a bubble to **dive in** — it's a live `claude` session.
- Press **`Ctrl+\` `Ctrl+\`** to pop back to the fleet.

Each bubble runs `claude` in its own folder (so it inherits that folder's
`CLAUDE.md` and `.claude/` setup).

### The fleet keeps running

`bubbles` runs your fleet in a **background daemon per directory**, so it stays
alive even when you close the IDE:

- **`q`** — **detach**: closes the IDE but leaves the whole fleet running (agents
  keep working). Run `bubbles` again from the same directory to reattach.
- **`Ctrl+]`** — stop the fleet entirely (every bubble).
- **`bubbles stop`** — stop a detached fleet without reattaching.
- **`bubbles --local`** — run once in the foreground with no daemon (closing it
  stops the fleet); handy for a quick session.

The fleet is also saved to disk and resumes (`claude --resume`) if the daemon is
ever stopped and you reopen.

## Keys

**Fleet view**

| Key | Action |
|---|---|
| `↑`/`↓` | move cursor (cyclable — wraps top/bottom) |
| `→`/`←` | expand / collapse a node (root starts collapsed, at the bottom) |
| `Enter` | dive into the bubble (or start root) |
| `n` | new bubble → persona → folder → **options: model (sonnet/opus/fable) + optional spawn grant** |
| `e` | **edit**: on a bubble, change persona / model / spawn grant; on a group header, add/remove member bubbles |
| `d` | **delete** the highlighted bubble and its subtree (with confirm) |
| `i` | introduce: add bubbles (`Enter`), `Enter` again on a ✓ to finalize |
| `g` | create a **group**: select bubbles → name it → options (introduce-all / attach a coordinator session) |
| `G` | delete a group (asks whether to also delete the member bubbles) |
| `0`–`9` | jump to a bound slot, or bind the highlighted bubble to a free one |
| `m` then `0`–`9` | (re)assign the highlighted bubble to a slot |
| `Ctrl+P` | toggle permission mode for new bubbles (allow-all ⇄ ask) |
| `q` | quit |

A `⚡` marks a bubble holding the **spawn grant** (it can spawn children, but at
depth 1 its children cannot). New bubbles default to the latest Sonnet; each keeps
its own model across restarts. If a bubble's `claude` crashes, it **self-heals** —
the next message to it (or diving in) relaunches it, resuming its conversation, or
starting fresh if the session id is gone.

**Inside a bubble** (`Ctrl+\` is the leader)

| Key | Action |
|---|---|
| `Ctrl+\` `Ctrl+\` | back to the fleet |
| `Ctrl+\` then `0`–`9` | jump to that slot (or bind the current bubble if free) |
| everything else | goes straight to `claude` (`Esc`, arrows, etc.) |

## How it works

The whole system bottoms out in one atom and one verb:

- **A Bubble** = an **address** (`0`, `0.1`, `0.1.2` — root is `0`) + a real `claude`
  session + the ability to **`send`**. Address = folder path = position in the tree.
- The IDE is a thin **control plane**: a Bubbletea TUI + a tiny kernel (addressing,
  capabilities, a message store) + a per-bubble **MCP bridge** that gives each
  `claude` session fleet-aware tools.

**Spawning & hierarchy.** You spawn bubbles under any node (`0.1` → `0.1.1`). A parent
can message its children. Only you (root) can grant the spawn capability.

**Messaging (no interruption).** Bubbles talk through inboxes, never by interrupting:

- `send(to, subject, body, reply_to?)` — files a message in a contact's inbox and
  returns an id; the recipient gets a non-interrupting "📬 you have mail" notice it
  picks up on its next turn.
- `inbox()` — read & clear unread (each shows sender `address (role)` and an id).
- `status()` — for messages you sent: `delivered` / `read, no reply` / `replied`, so
  an agent can decide whether to follow up instead of nagging.
- `contacts()` — who you can message. New bubbles know only root; use **`i`** to
  introduce others. A reply grant lets you always reply to whoever messaged you.

**Groups.** Press `g` to bundle any bubbles into a named **group** — pure
arrangement, independent of the folder tree, shown as a `{tag}` on members.
Optionally attach a coordinator `claude` session that can message every member, and
optionally introduce all members to each other on create. Groups are deletable
anytime (`G`) and deleting one never removes anyone's contacts.

**Persistence.** The fleet (addresses, personas, folders, contacts, number slots,
`claude` session ids) is saved to `<project>/.bubbles/fleet.json` and resumed with
`claude --resume` on reopen.

## Development

```bash
make test     # go test ./...   (kernel/TUI/MCP all covered, no claude needed)
make vet
make bin      # build ./bin/bubbles
```

The kernel never depends on real `claude` — a `FakeRunner` drives the whole
spawn/message/persist flow in tests, so the suite runs with zero tokens and no
network.

## Status & roadmap

MVP is working end-to-end: zoomable fleet, real `claude` bubbles, dive-in, nested
hierarchy, inbox messaging with read/reply status, persistence, and permission
toggle. On the roadmap: remote bubbles over SSH (run the fleet on a beefy VM),
channels/broadcast, in-dive message banners, and escalation policies.

- **bubbles-net** — connect your fleet to a friend's fleet over the internet and
  introduce bubbles across machines (E2E-encrypted, rendezvous server, no accounts).
  See [docs/WHATS-COMING.md](docs/WHATS-COMING.md).

## License

MIT — see [LICENSE](LICENSE).
