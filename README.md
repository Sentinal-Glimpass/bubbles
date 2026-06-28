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

**MVP 1, Plan 1 — the kernel — is built and fully tested.** The entire SMS + spawn
engine runs end-to-end against a `FakeRunner` with zero `claude`, zero tokens, zero
network. Next up (Plan 2): the Bubbletea TUI, a `LocalRunner` launching real
`claude`, and the MCP bridge.

Design and plans live in [`docs/superpowers/`](docs/superpowers/).

## Develop

```bash
go test ./...     # run the suite
go vet ./...      # static checks
make test         # same as above
```

The PTY spike (delivery de-risk) lives in `cmd/spike`; run it against a real `claude`
on macOS to confirm interrupt-delivery:

```bash
go run ./cmd/spike -cmd claude
```

## License

MIT — see [LICENSE](LICENSE).
