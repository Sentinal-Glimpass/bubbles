# Fix: transcript trimming must never destroy conversation history

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** A user lost a day of irreplaceable project work. Make the code physically incapable of causing that again, and make it loud if it tries.

**Architecture:** Turn the one destructive path in the codebase into a reversible one, gate it on facts that cannot go stale, and meter it. Then fix the stale state that made it dangerous.

**Tech Stack:** Go, stdlib. Branch `fix-transcript-safety` off main at `d4abee2`.

---

## The incident

On 2026-08-14 bubble `0.2` (markaible-dashboard) ran a major billing/chatbot overhaul with four other bubbles. Claude Code wrote 60 file-history versions and 8 shell snapshots for session `acfba5d9` that day. Its transcript's on-disk content ends **2026-07-31**. The conversation is gone. It was recovered only from the durable inbox store and file-history.

Established by investigation:
- **Bubbles never deletes a transcript.** Every `os.Remove` in the repo is a socket, pid file, temp config, or write-scratch file.
- **`importFleet` never ran** (the only code that overwrites a transcript wholesale).
- **`trimTranscript` (`cmd/bubbles/health.go:307`) permanently deletes** everything before the last compaction marker, via `.htrim` + rename. It is the only bubbles code that destroys transcript content.
- It picks its target with `convPath(home, b.Dir, b.SessionID)` — from a `SessionID` that **does not reliably persist** (`registry.SetSessionID` never bumps `r.version`, and `saveFleet` only runs on a version change). Four bubbles currently point at stale or nonexistent sessions.
- Exactly 4 transcripts were rewritten on 08-14 at 20:01–20:14, two of them 75 ms apart.

The root cause is not fully settled. **This plan does not depend on settling it** — it removes the ability to destroy data regardless of which hypothesis is right.

## Global Constraints

- **No existing test deleted or weakened.** If one fails, fix the change.
- Nothing on a sweep path may call `EnsureAlive`.
- Never hold a lock across file I/O, a PTY write, a launch, or a `Kill`.
- Every suppression/skip path increments a costmeter counter (standing law).
- New checks go in the inventory gate at `cmd/bubbles/checks_test.go`.
- Green: `go build ./... && go vet ./... && go test -count=1 ./...` plus `-race` on touched packages.
- `TestDaemonRelay` is a known pre-existing flake — re-run, don't chase.

---

## Task 1: trimming archives instead of deleting

**Files:** `cmd/bubbles/health.go`, tests alongside.

**The change:** the cut portion is **appended** to `<transcript>.jsonl.archive` and only once that write has succeeded is the live transcript replaced. Repeated trims accumulate in the archive; nothing is ever removed from it.

This is the whole point of the plan: bubbles wanted a smaller live transcript, and it can have that without irreversibility. Had this existed, the lost conversation would still be on disk.

- [ ] **Step 1: Write the failing tests.**
  - after a trim, the archive contains **exactly** the lines removed from the live file, and archive+live concatenated reconstruct the original byte-for-byte
  - a second trim **appends** to the existing archive rather than replacing it
  - **if the archive write fails, the live transcript is left completely untouched** — this is the test that matters; a trim that half-succeeds must not lose data
  - the archive is never itself trimmed, and is skipped by the transcript scan (it must not be mistaken for a session)
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Archive first, verify, then replace. Never the other order.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `fix(health): trimming archives the cut portion instead of deleting it`

---

## Task 2: two gates that cannot go stale

**Files:** `cmd/bubbles/health.go`, tests alongside.

`trimTranscripts` currently guards only on `m.k.IsHot(b.Addr)` — kernel state, which is exactly what was unreliable. Add two checks that depend on the **file itself**, not on the registry:

**Gate A — identity.** Claude Code records a `sessionId` inside transcript entries. Before rewriting, confirm it matches the bubble being trimmed for. A mismatch means the registry pointed at the wrong file; refuse, log, and count it.

**Gate B — recency.** Refuse to rewrite a transcript modified within `trimQuietPeriod` (5 minutes). A file being actively appended to must never be rewritten under the writer, whatever the kernel believes about hotness.

- [ ] **Step 1: Write the failing tests.**
  - a transcript whose internal `sessionId` differs from the bubble's is NOT rewritten, and the refusal is counted
  - a transcript modified 1 minute ago is NOT rewritten; one modified an hour ago is
  - a transcript with no `sessionId` field anywhere is NOT rewritten (unknown identity is not permission)
  - the existing `IsHot` guard still holds — this adds gates, it does not replace one
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.** Both gates are cheap reads; do them before `os.ReadFile` of the whole file where possible.
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `fix(health): gate trimming on the transcript's own identity and quiescence`

---

## Task 3: make every trim loud

**Files:** `cmd/bubbles/health.go`, `internal/costmeter/costmeter.go`, tests.

This incident required forensic archaeology because trimming is silent. That is the defect behind the defect.

Every trim attempt logs one line — path, bubble address, resolved session id, bytes before, bytes after, bytes archived, and the outcome (trimmed / refused-identity / refused-recent / refused-hot / no-boundary).

New counters, appended (never renumber): `FTranscriptsTrimmed`, `FTranscriptBytesArchived`, `FTrimsRefused`.

- [ ] **Step 1: Write the failing tests** — each outcome increments the right counter; a refusal is never silent; no `F*` constant is renumbered.
- [ ] **Step 2:** Confirm they fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4:** Tests pass.
- [ ] **Step 5: Commit** — `feat(costmeter): meter and log every transcript trim`

---

## Self-Review

Tasks 1–3 are one code path and should land together. Task 1 alone removes the data-loss capability; 2 removes the misidentification that likely triggered it; 3 makes the next occurrence a log line instead of an investigation.

**Deliberately NOT in this plan:** the `SetSessionID` persistence fix (separate branch, different blast radius) and pinning `cleanupPeriodDays` (a settings change, not code).

**Risk note:** Task 1 changes a delete into a write, so disk grows where it previously shrank. That is the intended trade — hourly backups already exist, and the operator can prune archives. Do not add automatic archive pruning; an auto-pruner would reintroduce exactly the class of bug this plan exists to remove.
