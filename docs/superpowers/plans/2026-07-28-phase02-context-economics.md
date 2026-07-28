# Phase 2: Context Economics — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound every bubble's context growth — measure real context size from the transcript, nudge a bubble to compact at 500k and force it at 800k, and stop billing every bubble for prompt text describing tools it does not have.

**Architecture:** A new pure `internal/transcript` package reads context size from a Claude Code `.jsonl` with no I/O policy of its own. The existing `HealthManager.Sweep()` gains checks that feed `costmeter` and drive the compaction pump through the Phase 1 notification path, so the pump inherits INV-1 and the costmeter for free rather than writing to PTYs directly.

**Tech Stack:** Go, stdlib only. Builds on Phase 0+1 (`internal/notify`, `internal/costmeter`), merged at `cef78de`.

**Spec:** `docs/superpowers/specs/2026-07-28-bubbles-cost-efficiency-design.md` §6

## Global Constraints

- **The pump must not become its own cost leak.** Every nudge costs a turn. Throttle per bubble and route through the Phase 1 policy so INV-1 (6 notices/bubble/min) applies. A pump that fires every sweep is worse than the problem.
- **`internal/transcript` is pure:** no `time.Now()`, no writes, no kernel import. It reads a file and returns numbers.
- **Only COLD bubbles' transcripts may be rewritten.** Claude appends through a held fd; rewriting a file it has open loses appends or corrupts it. This is pre-existing law in `trimTranscripts` — do not weaken it.
- **No message is ever dropped**; `UnreadCount` stays truthful (carried from Phase 1).
- **Every suppression path increments a costmeter counter** (carried from Phase 1).
- "No existing test is deleted or weakened to make new code pass." If a pre-existing test fails and you believe it encodes superseded behaviour, STOP and report the specific assertion.
- File ceiling ~400 lines for anything created or substantially rewritten.
- `go build ./... && go vet ./... && go test ./...` green at every task boundary, plus `-race` on touched packages.

### Context-size definition (measured against real transcripts)

A Claude Code assistant entry carries:

```json
"usage":{"input_tokens":131,"cache_creation_input_tokens":667,"cache_read_input_tokens":121801,"output_tokens":602,...}
```

**Context size = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`** of the LAST assistant entry — that is the full prompt the model was billed for on its most recent turn. `output_tokens` is NOT part of context size. Verified against a live transcript (131 + 667 + 121801 ≈ 122.6k).

### Thresholds

- `ContextNudgeTokens = 500_000` — inject a notice asking the bubble to `compact()` at its next checkpoint.
- `ContextForceTokens = 800_000` — kernel calls `Compact` itself.
- `ResumeSummaryThreshold` (`internal/runner/local.go:158`) is already `500_000`. It and `ContextNudgeTokens` must be **one shared constant**, not two literals that can drift.

---

## DELIBERATE SPEC DEVIATION — read before Task 4

Spec §6 says: *"Add a byte-ceiling trim path for never-compacted transcripts."* **Do not implement that.** It is unsafe and this plan replaces it.

`trimTranscript` today cuts before the latest `"isCompactSummary":true` marker, and that is safe for a precise reason recorded in its own comment: everything after a compaction boundary is *a self-contained conversation tree rooted at a `parentUuid:null` entry*. A never-compacted transcript has no such boundary anywhere. Cutting it at an arbitrary byte or line offset orphans the `parentUuid` chain of every surviving entry, which risks corrupting the conversation on `--resume` — trading a token cost for a data-loss bug.

**The pump is the correct fix for the never-compacted case.** Forcing `Compact` at 800k *creates* a compaction boundary, after which the existing, safe `trimTranscript` reclaims the space on the next sweep. So Task 3 and Task 4 compose: the pump makes the runaway compactable, and the existing trimmer then trims it.

Task 4 therefore adds *observability* for oversized never-compacted transcripts, not truncation.

---

## File Structure

**Create:**
- `internal/transcript/transcript.go` — pure reader: context size + compaction presence (~120 lines)
- `internal/transcript/transcript_test.go`
- `cmd/bubbles/contextpump.go` — the sweep check that nudges/forces (~150 lines)
- `cmd/bubbles/contextpump_test.go`

**Modify:**
- `internal/runner/local.go` — take the shared threshold constant
- `cmd/bubbles/health.go` — register the new checks in `Sweep()`
- `cmd/bubbles/citizen.go` — gate spawn/task prose on capability
- `cmd/bubbles/app.go` — pass what the pump needs

---

## Task 1: `internal/transcript` — pure transcript reader

**Files:**
- Create: `internal/transcript/transcript.go`, `internal/transcript/transcript_test.go`

**Interfaces:**
- Produces: `Stats{ContextTokens int64; HasCompaction bool; Entries int; Bytes int64}`; `Read(path string) (Stats, error)`; `ErrNoUsage`.

- [ ] **Step 1: Write the failing test**

Write tests using a temp `.jsonl` fixture you construct in the test (do NOT read the user's real transcripts). Cover:

```go
func TestContextTokensSumsTheLastAssistantEntry(t *testing.T) {
	// two assistant entries; the LAST one must win
	lines := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":999}}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":1000,"output_tokens":500}}}`,
	}
	p := writeFixture(t, lines)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextTokens != 1003 { // 1+2+1000; output_tokens excluded
		t.Fatalf("ContextTokens = %d, want 1003", got.ContextTokens)
	}
}

func TestOutputTokensAreNotContext(t *testing.T) { /* single entry, huge output_tokens, assert excluded */ }

func TestHasCompactionDetectsTheMarker(t *testing.T) {
	// one file containing "isCompactSummary":true -> true; one without -> false
}

func TestTrailingNonAssistantEntriesDoNotHideUsage(t *testing.T) {
	// assistant entry followed by a user entry: the assistant's usage must still be found
}

func TestNoUsageAnywhereReturnsErrNoUsage(t *testing.T) { /* user-only transcript */ }

func TestMalformedLinesAreSkippedNotFatal(t *testing.T) {
	// a truncated/garbage line between two valid entries must not abort the read
}
```

- [ ] **Step 2: Run the test, confirm it fails** (`PATH=$PATH:/home/rishi/goroot/bin go test ./internal/transcript/ -v`) — package does not exist.

- [ ] **Step 3: Implement**

- Scan line by line (`bufio.Scanner` with an enlarged buffer — transcript lines can exceed the 64KB default and a silent `ErrTooLong` would under-report context, which is a correctness bug here).
- Decode each line into a minimal anonymous struct; a line that fails to decode is SKIPPED, not fatal.
- Track the last entry that carried a `usage` object; `ContextTokens` = its `input + cache_creation + cache_read`.
- `HasCompaction` = any line contains `"isCompactSummary":true` (reuse the same byte signature `cmd/bubbles/health.go` uses; do not invent a second one).
- `Bytes` = file size; `Entries` = decoded line count.
- Return `ErrNoUsage` when no entry carried usage.

- [ ] **Step 4: Tests pass; `go test ./...` green.**
- [ ] **Step 5: Commit** — `feat(transcript): pure reader for context size and compaction state`

---

## Task 2: single shared context threshold

**Files:** Modify `internal/runner/local.go`; create the constant wherever both consumers can see it without an import cycle (`internal/transcript` is a reasonable home — `runner` may import it; it must NOT import `runner`).

- [ ] **Step 1:** Write a test asserting `runner.NewLocal().ResumeSummaryThreshold == transcript.ContextNudgeTokens`, so the two can never drift.
- [ ] **Step 2:** Confirm it fails (constant does not exist).
- [ ] **Step 3:** Define `ContextNudgeTokens = 500_000` and `ContextForceTokens = 800_000` once; make `local.go:158` use the shared constant instead of its literal.
- [ ] **Step 4:** `go test ./...` green.
- [ ] **Step 5:** Commit — `refactor(runner): share the 500k context threshold instead of duplicating it`

---

## Task 3: the compaction pump

**Files:** Create `cmd/bubbles/contextpump.go`, `cmd/bubbles/contextpump_test.go`; modify `cmd/bubbles/health.go` (`Sweep`).

**Interfaces:**
- Consumes: `transcript.Read`, `kernel.Compact`, `costmeter`, the Phase 1 notification path.
- Produces: `(*HealthManager).pumpContext()` registered in `Sweep()`.

- [ ] **Step 1: Write the failing tests**

Cover, with a fake/synthesised transcript per bubble:
- below 500k → nothing happens (no notice, no compact)
- ≥500k → exactly one nudge, and `FContextTokens` recorded
- ≥500k on a second sweep soon after → NO second nudge (throttle holds)
- ≥800k → `Compact` invoked
- a bubble with no transcript / `ErrNoUsage` → skipped silently, no crash
- the nudge routes through the Phase 1 path, so a bubble already at its INV-1 ceiling gets no extra notice

- [ ] **Step 2:** Confirm the tests fail.

- [ ] **Step 3: Implement**

- For each registered bubble with a `SessionID` and `Dir`, resolve its transcript via the existing `convPath(home, dir, sessionID)` in `cmd/bubbles/portable.go:106` — do not reimplement path resolution.
- `transcript.Read` it. On error other than a genuine problem worth reporting, skip quietly.
- Always record `costmeter.Set(addr, FContextTokens, stats.ContextTokens)` — this is the gauge the TUI panel and later phases consume. Recording is unconditional even when no action is taken.
- `>= ContextForceTokens` → call `k.Compact(addr, "")`. Rate-limit per bubble.
- `>= ContextNudgeTokens` → send a notice through the **Phase 1 notification path**, not a raw PTY write, so it inherits INV-1, the costmeter, and the muted/typing-hold discipline. Wording should state the measured size and ask the bubble to call `compact()` at its next natural checkpoint.
- **Throttle per bubble** (a named constant, e.g. 30 minutes) so a bubble sitting above the threshold is not nudged every sweep. The sweep runs every 2 minutes; an unthrottled pump would cost ~30 turns/hour/bubble — worse than the leak it fixes.
- Hot vs cold: reading a transcript is safe on both (read-only). Only ACT on a bubble that is hot, or that the notification path would legitimately reach; do not page in a cold bubble just to tell it to compact — that would pay the rewarm this project exists to avoid.

- [ ] **Step 4:** Tests pass; `go test ./...` green; `-race` on `./cmd/bubbles/`.
- [ ] **Step 5: Commit** — `feat(health): tiered context pump — nudge at 500k, force compaction at 800k`

---

## Task 4: observability for never-compacted runaways

**Files:** Modify `cmd/bubbles/health.go`; test alongside.

**Read the DELIBERATE SPEC DEVIATION section above before starting.** You are NOT adding truncation.

- [ ] **Step 1: Write the failing test** — a cold bubble whose transcript is large and has no compaction marker is reported (counter and/or a single stderr line), and its file is left BYTE-IDENTICAL. Assert the file's bytes are unchanged; that assertion is the point of the task.
- [ ] **Step 2:** Confirm it fails.
- [ ] **Step 3: Implement** — in the existing `trimTranscripts` sweep, when `trimTranscript` finds no compaction boundary and the file exceeds a byte ceiling, record it (a costmeter counter and one throttled stderr line). Do not modify the file. Add a comment stating explicitly why truncation is unsafe here (orphaned `parentUuid` chain), so a future contributor does not "finish the job".
- [ ] **Step 4:** Tests pass; `go test ./...` green.
- [ ] **Step 5: Commit** — `feat(health): report never-compacted oversized transcripts instead of unsafely truncating`

---

## Task 5: gate the citizen prompt on capability

**Files:** Modify `cmd/bubbles/citizen.go`, and whatever composes the prompt at launch; test alongside.

**Context:** MCP tool *schemas* are already gated on `Caps.CanSpawn` (see `cmd/bubbles/app.go` where `mcpConfigJSON` receives `k.Caps.CanSpawn(a)`), but `citizenPrompt` describes `spawn`/`edit`/`delete`/`introduce`/`broadcast`/`assign_task` in prose to EVERY bubble. A leaf worker pays for that text on every turn describing tools it does not have.

- [ ] **Step 1: Write the failing test** — a prompt built for a non-spawner contains none of `spawn(`, `edit(`, `delete(`, `introduce(`, `broadcast(`, `assign_task(`; a prompt built for a spawner contains all of them. Assert the non-spawner prompt is materially shorter.
- [ ] **Step 2:** Confirm it fails.
- [ ] **Step 3: Implement** — split `citizenPrompt` into a base section plus a spawn section, composed per bubble from the SAME `Caps.CanSpawn` value that already gates the tool schemas. Do not introduce a second notion of "may spawn" — take the existing one, or the prompt and the tool list will drift.
- [ ] **Step 4:** Tests pass; `go test ./...` green.
- [ ] **Step 5: Commit** — `perf(citizen): gate spawn/task prose on capability to match the tool gating`

---

## Self-Review

**Spec §6 coverage:** token source → Task 1; shared constant → Task 2; tiered pump (500k nudge / 800k force) → Task 3; never-compacted blind spot → Task 4 (deliberately as observability, not truncation — rationale above); citizen prompt gating → Task 5.

**Deliberate deviation:** §6's byte-ceiling truncation of never-compacted transcripts is NOT implemented; it would orphan the `parentUuid` chain. The pump supersedes it by creating a compaction boundary the existing safe trimmer can then use.

**Not in this plan:** Phase 3 (cache-aware paging) and Phase 4 (health hardening) get their own plans.
