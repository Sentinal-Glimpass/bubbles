# Bubbles MVP 1 — Plan 2: The Visible Layer (TUI + real claude + MCP)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make the kernel visible and real — a Bubbletea zoomable tree (blink pings, dive-in/`esc`), a `LocalRunner` that launches real `claude` sessions in PTYs, and an MCP bridge that gives each session the `send`/`contacts`/`spawn` tools wired back to the kernel.

**Architecture:** The kernel from Plan 1 is unchanged. Three new layers sit on top, each behind a clean boundary so it's testable without a real `claude`: (1) `tui` — a Bubbletea model reading the registry and bus, tested with `teatest`; (2) `runner.LocalRunner` — `creack/pty` + `os/exec` launching `claude`, tested against a stub command; (3) `mcpstdio` — a minimal stdio JSON-RPC MCP server (one helper subprocess per bubble) that relays tool calls to the main process over a unix socket, tested with in-memory pipes. The `cmd/bubbles` binary wires them; a hidden `bubbles mcp-stdio` subcommand is the per-bubble MCP helper.

**Tech Stack:** Go; `github.com/charmbracelet/bubbletea` + `lipgloss` (TUI); `github.com/charmbracelet/x/exp/teatest` (TUI tests); `github.com/creack/pty` (PTYs); stdlib `net`, `encoding/json` (MCP + socket). No MCP SDK — the stdio JSON-RPC server is hand-rolled and dependency-free.

## Global Constraints

- Module `github.com/Sentinal-Glimpass/bubbles`, Go ≥ 1.22 (same as Plan 1).
- **Interrupt byte = `0x1b` (Esc)** for `claude` (confirm via `cmd/spike -cmd claude` on macOS; configurable in `LocalRunner`).
- **Root (the human) acts through the TUI, not through MCP.** So MCP exposes `send` + `contacts` to every bubble, and `spawn` only to bubbles root granted it. `introduce` stays a root/TUI action (never an MCP tool).
- Each bubble launches as: `claude --append-system-prompt <citizen> --permission-mode acceptEdits --mcp-config <json> "<persona+goal>"` run in the bubble's directory.
- Anything requiring a live `claude` is validated on macOS (see "Mac validation" at the end), mirroring the Plan 1 spike. Headless tests must never need `claude`, tokens, or the network.

---

### Task 1: `tui` — Bubbletea zoomable fleet tree

**Files:**
- Create: `internal/tui/model.go` (the `tea.Model`: state, Update, View)
- Create: `internal/tui/view.go` (tree rendering with lipgloss, blink)
- Create: `internal/tui/msgs.go` (bus→tui message adapters)
- Test: `internal/tui/model_test.go` (teatest)

**Interfaces:**
- Consumes: `registry.Registry` (read tree/status), `bus.Bus` (root inbox → ping messages), `kernel.Kernel` (issue Spawn/Introduce/Send from key actions).
- Produces:
  - `type Model struct { ... }`
  - `New(k *kernel.Kernel) Model`
  - `type pingMsg struct { From addr.Address; Subject string }` delivered via `tea.Program.Send`
  - Key map: `↑/↓` move cursor, `enter` request dive-in (emits `DiveMsg{Addr}`), `esc` back, `n` new bubble, `q` quit.
  - Blink: a bubble with an unread ping renders with a `●` marker + inverted subject box until visited; a `tea.Tick` toggles the highlight.

**Behavioral spec (what the tests assert):**
- Initial view lists root and its children with status dots.
- A `pingMsg` from `0.1` makes row `0.1` show its subject and a blink marker.
- Pressing `enter` on a row emits a `DiveMsg` with that row's address (the live PTY handoff itself lives in `cmd/bubbles`, not the model — the model just signals intent).
- Pressing `n`, typing a persona, and `enter` calls `kernel.Spawn(addr.Root, persona, dir, opts)` and the new row appears.

- [ ] **Step 1: Add deps**
```bash
go get github.com/charmbracelet/bubbletea@latest github.com/charmbracelet/lipgloss@latest
go get github.com/charmbracelet/x/exp/teatest@latest
```

- [ ] **Step 2: Write the failing teatest** (`model_test.go`): start the model with a kernel over a `FakeRunner`, spawn two bubbles, send a `pingMsg`, assert the rendered output (via `teatest.WaitFor`) contains the persona names, the ping subject, and the blink marker; send `q` and assert it finishes.

- [ ] **Step 3: Run → FAIL** (`go test ./internal/tui/`), build error.

- [ ] **Step 4: Implement** `model.go`/`view.go`/`msgs.go` to satisfy the spec. Model holds: `k *kernel.Kernel`, ordered `rows []addr.Address` (rebuilt from `k.Reg`), `cursor int`, `focus *addr.Address`, `pings map[addr.Address]string`, `blinkOn bool`. `Update` handles `tea.KeyMsg`, `pingMsg`, `blinkTickMsg`. `View` renders the tree with lipgloss; blinking rows use a highlighted style when `blinkOn` and a ping is unread.

- [ ] **Step 5: Run → PASS.**

- [ ] **Step 6: Commit** (`feat(tui): bubbletea zoomable fleet tree with blink pings`).

---

### Task 2: `runner.LocalRunner` — launch real `claude` in a PTY

**Files:**
- Create: `internal/runner/local.go`
- Test: `internal/runner/local_test.go` (against a stub command, not `claude`)

**Interfaces:**
- Consumes: `addr.Address`, `SpawnOpts`, `creack/pty`.
- Produces:
  - `type LocalRunner struct { Bin string; CitizenPrompt string; MCPConfig func(addr.Address) string; InterruptByte byte }`
  - `NewLocal() *LocalRunner` (defaults: `Bin:"claude"`, `InterruptByte:0x1b`)
  - implements `Runner`: `Launch(a, dir, opts) (Session, error)`, `Kill(a) error`.
  - `Launch` builds `exec.Command(Bin, "--append-system-prompt", CitizenPrompt, "--permission-mode", "acceptEdits", "--mcp-config", MCPConfig(a), initialPrompt(opts))`, sets `cmd.Dir = dir`, starts it under `pty.Start`, returns a `*ptySession`.
  - `type ptySession` implements `Session`: `Write` injects `InterruptByte` then the bytes then `\r` (interrupt-delivery); `Close` kills the process. It also exposes `PTY() *os.File` so `cmd/bubbles` can do the dive-in handoff.

**Test approach (headless):** set `Bin` to a stub shell script that echoes its args and reads stdin, so `Launch` → `Write` → `PTY()` round-trips are asserted without `claude`. The stub proves: args are assembled correctly, `cmd.Dir` is honored, `Write` reaches the process, `Kill` terminates it.

- [ ] **Step 1: Write failing `local_test.go`** using a temp stub script (`#!/bin/sh\necho ARGS:"$@"\ncat`) as `Bin`; assert the PTY output contains `ARGS:` with the expected flags and the injected text.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement `local.go`.**
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** (`feat(runner): LocalRunner launching processes in a PTY`).

---

### Task 3: `mcpstdio` — minimal stdio JSON-RPC MCP server

**Files:**
- Create: `internal/mcpstdio/server.go` (JSON-RPC 2.0 over an `io.Reader`/`io.Writer`)
- Create: `internal/mcpstdio/tools.go` (the 4 tool schemas + dispatch to a `Backend` interface)
- Test: `internal/mcpstdio/server_test.go` (drive with raw JSON-RPC over `io.Pipe`)

**Interfaces:**
- Produces:
  - `type Backend interface { Send(from, to, subject, body string) error; Contacts(owner string) []string; Spawn(by, persona, dir string) (string, error) }` — implemented in `cmd/bubbles` by relaying to the kernel over the unix socket; in tests by a fake.
  - `type Server struct { Self string; B Backend }` (`Self` = this bubble's address from `BUBBLE_ADDR`).
  - `(*Server) Serve(in io.Reader, out io.Writer) error` — reads newline-delimited JSON-RPC, handles `initialize` (returns protocolVersion + serverInfo + `tools` capability), `notifications/initialized` (no reply), `tools/list` (returns `send`/`contacts`, plus `spawn` when granted), `tools/call` (dispatches to `Backend`, with `from`/`by` forced to `Self` so a bubble can't spoof identity).

**Test approach (headless):** pipe `initialize` → assert result; `tools/list` → assert tool names; `tools/call send {to,subject,body}` → assert the fake `Backend` recorded a send from `Self`; a malformed call → assert a JSON-RPC error object.

> **Mac validation:** `initialize`'s `protocolVersion` and capability shape must match what the installed `claude` expects. Confirm by launching one real bubble and checking `claude` lists the tools (see Mac validation). If the version string needs bumping, it's a one-line change in `server.go`.

- [ ] **Step 1: Write failing `server_test.go`** (fake Backend, `io.Pipe`, the three exchanges above).
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement `server.go` + `tools.go`.**
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** (`feat(mcpstdio): minimal stdio JSON-RPC MCP server`).

---

### Task 4: unix-socket relay between helper and main process

**Files:**
- Create: `internal/ipc/ipc.go` (length-prefixed JSON request/reply over a unix socket)
- Test: `internal/ipc/ipc_test.go`

**Interfaces:**
- Produces:
  - `type Request struct { Op string; From, To, Subject, Body, Persona, Dir string }`
  - `type Reply struct { OK bool; Err string; Contacts []string; Addr string }`
  - `Serve(sock string, handle func(Request) Reply) (io.Closer, error)` — main process listens.
  - `Dial(sock string) (*Client, error)`; `(*Client) Do(Request) (Reply, error)` — helper calls.

**Test approach (headless):** real unix socket in a temp dir; `Serve` with a handler that echoes; `Dial` + `Do` round-trips a `Request`/`Reply`. Fully deterministic, no `claude`.

- [ ] Steps 1–5: failing test → implement → pass → commit (`feat(ipc): unix-socket relay for bubble MCP helpers`).

---

### Task 5: `cmd/bubbles` — wire the binary + `mcp-stdio` subcommand + dive-in

**Files:**
- Create: `cmd/bubbles/main.go` (subcommand dispatch: default = TUI app; `mcp-stdio` = MCP helper)
- Create: `cmd/bubbles/app.go` (build kernel+LocalRunner+TUI, IPC server, MCPConfig generator, dive-in PTY handoff)
- Create: `cmd/bubbles/citizen.go` (the `bubble-citizen` system-prompt text)

**Interfaces / wiring:**
- `main`: if `os.Args[1] == "mcp-stdio"` → run `mcpstdio.Server{Self: $BUBBLE_ADDR, B: ipcBackend{Dial($BUBBLE_SOCK)}}.Serve(os.Stdin, os.Stdout)`. Else → run the app.
- `app`: create `kernel.New(localRunner)`; `localRunner.MCPConfig = func(a) string{ return inline JSON: stdio server, command=<self exe>, args=["mcp-stdio"], env={BUBBLE_ADDR:a, BUBBLE_SOCK:sock} }`; start `ipc.Serve(sock, handle)` where `handle` maps `Request` → kernel `Send/Contacts/Spawn` (identity forced to `Request.From/By`); run Bubbletea with the TUI model.
- **Dive-in handoff:** on `DiveMsg{addr}`, suspend Bubbletea (`tea.Program.ReleaseTerminal`), copy `os.Stdin↔ptySession.PTY()` until a detach chord (`Ctrl-\`, byte `0x1c`), then `RestoreTerminal` and resume. (tmux-style; no embedded vt100 emulator in v1.)
- `citizen.go`: a concise system prompt — "You are bubble `<addr>` in a fleet. Use the `send` MCP tool to message contacts (root is `0`); keep subjects short. Use `contacts` to see who you can reach." (addr is injected by appending to the prompt per-launch.)

**Test approach:** `go build ./...` must succeed; `go vet ./...` clean. The IPC handler's kernel mapping is covered by Task 4 + kernel tests. Live behavior is Mac-validated.

- [ ] **Step 1:** implement `citizen.go`, `app.go`, `main.go`.
- [ ] **Step 2:** `go build ./... && go vet ./...` → clean.
- [ ] **Step 3:** `go build -o bin/bubbles ./cmd/bubbles` → single binary.
- [ ] **Step 4: Commit** (`feat(cmd): bubbles binary wiring TUI+runner+MCP, with mcp-stdio helper`).

---

### Task 6: single-binary build + README refresh

- [ ] Add a `make build` target producing `bin/bubbles`; gitignore `bin/`.
- [ ] Update README status to "Plan 2 built; live `claude` wiring pending Mac validation," with run instructions.
- [ ] Commit (`docs: Plan 2 build + run instructions`).

---

## Mac validation (needs a real `claude`)

Run on macOS where `claude` is installed:

1. **Spike interrupt byte:** `go run ./cmd/spike -cmd claude` then `-int 0x1b`. Confirm Esc interrupts a turn and a follow-up lands. Update `LocalRunner.InterruptByte` if needed; fill in `docs/superpowers/SPIKE-pty.md`.
2. **MCP handshake:** `./bin/bubbles`, spawn one bubble, and confirm that bubble's `claude` lists the `send`/`contacts` tools (`/mcp` inside the session, or it uses them). If `initialize.protocolVersion` is rejected, bump it in `mcpstdio/server.go`.
3. **End-to-end:** spawn two bubbles, `introduce` them from the TUI, have one `send` the other, confirm the message is injected into the recipient's PTY and that a worker→root `send` blinks the row.
4. **Dive-in:** `enter` a bubble, chat live, `Ctrl-\` to detach back to the fleet.

## What Plan 2 delivers (after Mac validation)

A runnable `bubbles` binary: a live zoomable fleet of real `claude` agents, blink pings to root, dive-in/detach, and inter-agent `send` over an MCP bridge — the MVP 1 experience end to end.
