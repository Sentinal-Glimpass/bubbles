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

The one-command installer below sets all of these up for you; they're listed here
for reference:

- **[Claude Code](https://claude.com/claude-code)** — the `claude` CLI (Bubbles
  launches real `claude` sessions). You still authenticate it once (`claude`, sign in).
- **Go 1.25+** — build toolchain (runtime doesn't need it).
- **ngrok** *(optional)* — only for public webhook URLs (`bubbles --ngrok`).
- **Linux** with systemd `--user` gives per-bubble memory accounting; without it,
  bubbles still runs (uncapped).

## Install

**One command** — installs every dependency (Go, Claude Code, ngrok) and builds
bubbles, all under `$HOME`, no sudo:

```bash
curl -fsSL https://raw.githubusercontent.com/Sentinal-Glimpass/bubbles/main/install.sh | bash
```

It's idempotent (skips anything already present) and drops `bubbles` in
`~/.local/bin`. Add `| bash -s -- --no-ngrok` to skip the optional ngrok install.
Then authenticate Claude Code once (`claude`, sign in) and run `bubbles`.

<details>
<summary>Manual options</summary>

```bash
# Go module install (needs Go 1.25+ on your PATH):
go install github.com/Sentinal-Glimpass/bubbles/cmd/bubbles@main   # -> $(go env GOPATH)/bin

# Or from source:
git clone https://github.com/Sentinal-Glimpass/bubbles.git && cd bubbles
make bootstrap      # deps + build  (ARGS="--no-ngrok" to skip ngrok)
make install        # just build -> ~/.local/bin/bubbles
```

Ensure the install dir is on your `PATH` (`~/.local/bin` or `$(go env GOPATH)/bin`).
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

## Backends: subscription, API key, or AWS Bedrock / Vertex

Bubbles isn't tied to a Claude subscription — it just launches the `claude` CLI,
so it uses **whatever backend Claude Code is configured for**. The launch
environment is inherited all the way to each bubble's session, so you configure
it the standard Claude Code way and start bubbles.

**AWS Bedrock** — just export the standard vars and start bubbles. Claude Code
auto-resolves the `sonnet`/`opus`/`haiku` aliases to the right Bedrock model IDs
(cross-region inference profiles picked from your `AWS_REGION`), so you **don't
need to specify a model**:

```bash
export CLAUDE_CODE_USE_BEDROCK=1
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=...          # or an AWS_PROFILE / instance role
export AWS_SECRET_ACCESS_KEY=...

bubbles stop     # if a daemon from a previous (subscription) session is running
bubbles
```

- **That's it** — `sonnet`/`opus` in the spawn picker and the default all resolve
  to Bedrock automatically. (Optional: pin a specific model or inference-profile
  ARN with `bubbles --default-model <id>`, or `--default-model auto` to pass no
  `--model` at all and let `ANTHROPIC_MODEL` decide.)
- **Restart matters:** a running daemon keeps the env it started with, so set the
  vars *before* `bubbles` (run `bubbles stop` first if one is already up).
- **API key** instead of subscription: `export ANTHROPIC_API_KEY=...`. **Vertex
  AI**: same as Bedrock but `CLAUDE_CODE_USE_VERTEX=1` + the Google vars.
- The top-right **Claude usage panel is subscription-only** (it reads the
  subscription's `/usage`); on Bedrock/Vertex it simply hides itself — billing is
  on your cloud account.

## Token compression (optional)

A busy fleet burns tokens fast. Bubbles can route every session through
[Headroom](https://github.com/headroomlabs-ai/headroom), a local compression
proxy that shrinks tool outputs, logs, and history before they reach the model
(and trims what the model writes back). Same answers, fewer tokens.

```bash
uv tool install "headroom-ai[proxy]"    # one-time: install the proxy
bubbles --headroom                      # route the whole fleet through it
```

- **One proxy, whole fleet** — bubbles starts it, health-checks it, and points
  every session at it (`ANTHROPIC_BASE_URL`, or `ANTHROPIC_BEDROCK_BASE_URL` +
  `--backend bedrock` when `CLAUDE_CODE_USE_BEDROCK=1`). Off by default.
- **Fail-open** — routing turns on only after the proxy reports healthy; if it's
  not installed or won't start, the fleet talks to the provider directly. If the
  proxy crashes, bubbles relaunches it.
- **Cache-safe** — Headroom compresses only new bytes and keeps the frozen prefix
  byte-identical, so provider prompt caches still hit.
- Port defaults to 8787 (`--headroom-port`). Logs: `.bubbles/headroom.log`. See
  savings with `headroom dashboard` / `headroom stats`.

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
