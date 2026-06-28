# Spike: PTY interrupt-delivery into a live session

**Date:** 2026-06-28
**Machine / version:** Linux x86_64 (Go 1.22.5). `claude` not installed here — see "Pending on Mac".

## What was tested
`cmd/spike` launches a command in a PTY (`github.com/creack/pty`), injects a prompt,
lets the task run, sends an interrupt byte mid-task, then injects a follow-up.

Validated against `bash` (stand-in for `claude` on this Linux box):
```
go run ./cmd/spike -cmd bash \
  -prompt 'echo TASK-STARTED; sleep 30; echo TASK-FINISHED-SHOULD-NOT-APPEAR' -int 0x03
```

## Observations (bash stand-in)
- Prompt injection works: **yes** — `TASK-STARTED` printed.
- Session begins working: **yes** — `sleep 30` ran.
- Interrupt byte 0x03 (Ctrl-C) stops the running task: **yes** — `^C`, returned to
  prompt, and `TASK-FINISHED-SHOULD-NOT-APPEAR` never printed (the sleep was killed).
- Follow-up after interrupt lands: **yes** — the next line was delivered and executed.

**Conclusion (plumbing):** the PTY launch → inject → interrupt → follow-up loop is
solid and works exactly as the interrupt-delivery design (§5) needs.

## Pending on Mac (the `claude`-specific verdict)
Run the same spike against the real CLI on macOS:
```
go run ./cmd/spike -cmd claude                 # try Ctrl-C
go run ./cmd/spike -cmd claude -int 0x1b       # try ESC if Ctrl-C doesn't interrupt
```
Answer there:
- Which byte interrupts a live `claude` turn — `0x03` (Ctrl-C) or `0x1b` (ESC)?
- After interrupt, does the injected follow-up become a new instruction (vs being
  swallowed / requiring a submit key like a second `\r`)?

## Verdict for Plan 2
- [x] **PTY plumbing is viable** (bash-validated) → Plan 2's `LocalRunner` uses
      `creack/pty` for launch + injection.
- [ ] **Interrupt-delivery on `claude` confirmed** → set the interrupt byte from the
      Mac result; if `claude` proves unreliable to interrupt cleanly, adopt the
      Stop-hook turn-boundary fallback (a Claude Code Stop hook drains the inbox at
      end of each turn). Decision deferred to the Mac run; both paths are supported
      by the same `Runner` boundary, so this does not block Plan 1.

## Notes
- Bash paste-mode markers (`[?2004h`/`[?2004l`) appeared in raw output; the TUI
  (Plan 2) will own a proper terminal renderer, so these are cosmetic here.
- Total spike runtime is fixed (~12s of sleeps); fine for a manual spike.
