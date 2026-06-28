# Bubbles

> An agent-native terminal IDE. Mission control for a fleet of Claude Code agents.

Traditional IDEs are built around *one human typing into files*. **Bubbles** is built
around *many agents working in parallel*. Each agent is a real Claude Code session
rendered as a **bubble** in a sleek, zoomable terminal tree. Your job shifts from
*writing code* to *supervising a fleet*: spawn agents, watch them ping you when they
need you, dive into any one to collaborate, pop back out.

It lives entirely in the terminal. No Electron, no browser engine — a single native
binary that's a thin control plane over the agents it manages.

## The idea in one breath

One atom and one verb:

- **A Bubble** = an address + a session (a real `claude`) + the ability to **`send`**.
- Everything is `send` recomposed: a ping is `send(root, …)`, diving in is `send`
  back and forth, spawning is a `send` to the spawner.

And one tree seen three ways:

```
address      0.1.2
folder       <workspace>/0.1/0.1.2/
UI           click bubble 0 → see 0.1 → click → see 0.1.2
```

## Inter-agent comms: SMS, not Discord

- Every bubble has an address (its phone number) and an inbox.
- A bubble may only text addresses in its **contacts**. A fresh bubble knows **only
  you** (root).
- You (**root**) `introduce(A, B)` to make two bubbles peers. Peer comms exists only
  where you made an introduction. (Channels / broadcast are a v2 bow on top.)

## Status

**MVP 1 is built and fully tested headless.** Both layers are in:

- **Plan 1 — the kernel:** addressing, the message bus, capabilities (SMS contacts +
  spawn budget), the registry, and the runner abstraction. Driven end-to-end by a
  `FakeRunner` with zero `claude`, zero tokens, zero network.
- **Plan 2 — the visible layer:** a Bubbletea zoomable fleet tree (blink pings,
  dive-in/detach), a `LocalRunner` that launches real `claude` in PTYs, and an MCP
  bridge (`bubbles mcp-stdio` helper + unix-socket relay) giving each session the
  `send`/`contacts` tools. The compiled binary's MCP path is proven end-to-end in a
  test; the **live `claude` run is pending validation on macOS** (see below).

Design and plans live in [`docs/superpowers/`](docs/superpowers/).

## Run & develop

```bash
make test     # full suite (also: go test ./...)
make vet      # static checks
make bin      # build bin/bubbles
./bin/bubbles # launch the fleet TUI (needs claude on PATH for live bubbles)
```

Inside the TUI: `↑/↓` move · `enter` dive into a bubble · `Ctrl-\` detach · `n` new
bubble · `q` quit.

### macOS validation (needs a real `claude`)

The headless tests cover every layer with fakes; these steps confirm the live
integration (see `docs/superpowers/plans/2026-06-28-bubbles-mvp1-visible-layer.md`):

```bash
go run ./cmd/spike -cmd claude        # confirm Esc interrupts a turn (or -int 0x1b)
./bin/bubbles                         # spawn a bubble; /mcp should list send, contacts
```

## License

MIT — see [LICENSE](LICENSE).
