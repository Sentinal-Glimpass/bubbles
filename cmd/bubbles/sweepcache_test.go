package main

import (
	"os"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// TestSweepReadsEachTranscriptOnce pins the performance property as a
// behavioural one. Every sweep used to decode each bubble's whole transcript
// two or three times (pumpContext, the oversized check, and trimTranscript's
// own os.ReadFile); at the 4 MiB ceiling that is megabytes of I/O plus a full
// JSON decode per cold bubble every 2 minutes.
//
// The proof is indirect but exact: inside a sweep the file is deleted after the
// first read, so any second reader that actually touched the disk would fail.
func TestSweepReadsEachTranscriptOnce(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	f.writeContext(t, 123_000)
	b, _ := f.k.Reg.Get(f.a)
	path := convPath(f.home, f.dir, b.SessionID)

	f.m.beginSweep()
	st, err := f.m.transcriptStats(path)
	if err != nil || st.ContextTokens != 123_000 {
		t.Fatalf("first read: stats = %+v, err = %v", st, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	again, err := f.m.transcriptStats(path)
	if err != nil || again != st {
		t.Fatalf("second read inside the sweep re-read the file instead of reusing the decode: %+v, %v", again, err)
	}
	f.m.endSweep()

	// Outside a sweep the memo is gone: nothing keeps a decode alive across the
	// 2-minute gap, where the file can change under an mtime that did not move.
	if _, err := f.m.transcriptStats(path); err == nil {
		t.Fatal("the memo must not outlive the sweep that built it")
	}
}

// TestSweepStillPumpsAndTrimsFromOneRead: the memo must not change what either
// consumer decides. A cold, oversized, never-compacted transcript is still
// reported, and its context size is still gauged, from a single decode.
func TestSweepStillPumpsAndTrimsFromOneRead(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	f.writeOversizedNoCompaction(t)

	f.m.Sweep()

	if got := f.k.Cost.Snapshot()[f.a].OversizedTranscripts; got != 1 {
		t.Fatalf("OversizedTranscripts = %d, want 1", got)
	}
}

// TestOversizedReportCounterIsThrottledWithItsWarning: the counter is rendered
// beside genuine incident counters in the TUI. Incremented per sweep, one
// parked bubble accrued ~30/hour for a single unchanged condition, so
// "OversizedTranscripts: 214" read as 214 events when it meant one file.
func TestOversizedReportCounterIsThrottledWithItsWarning(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	f.writeOversizedNoCompaction(t)

	f.m.trimTranscripts()
	f.m.trimTranscripts()
	f.m.trimTranscripts()

	if got := f.k.Cost.Snapshot()[f.a].OversizedTranscripts; got != 1 {
		t.Fatalf("OversizedTranscripts = %d after 3 sweeps, want 1 -- the counter counts reports, not sweeps", got)
	}

	// It is still a report, not a one-shot: once the throttle expires the
	// condition is recorded again.
	f.m.reportMu.Lock()
	f.m.lastOversizedReport[f.a] = time.Now().Add(-oversizedReportThrottle - time.Minute)
	f.m.reportMu.Unlock()

	f.m.trimTranscripts()
	if got := f.k.Cost.Snapshot()[f.a].OversizedTranscripts; got != 2 {
		t.Fatalf("OversizedTranscripts = %d after the throttle expired, want 2", got)
	}
}

// TestSweepPrunesStateOfDeletedBubbles: lastPump and lastOversizedReport are
// keyed by address and were never cleaned up, so a fleet that spawns and
// deletes workers all day grew one entry per address ever seen.
func TestSweepPrunesStateOfDeletedBubbles(t *testing.T) {
	f := newPumpFixture(t)
	s := f.hot(t)
	_ = s
	f.writeContext(t, transcript.ContextForceTokens+50_000)

	f.m.Sweep()
	f.m.pumpMu.Lock()
	got := len(f.m.lastPump)
	f.m.pumpMu.Unlock()
	if got != 1 {
		t.Fatalf("setup: lastPump has %d entries, want 1", got)
	}

	f.k.DeleteBubble(f.a)
	f.m.Sweep()

	f.m.pumpMu.Lock()
	got = len(f.m.lastPump)
	f.m.pumpMu.Unlock()
	if got != 0 {
		t.Fatalf("lastPump kept %d entries for a deleted bubble", got)
	}
	f.m.reportMu.Lock()
	n := len(f.m.lastOversizedReport)
	f.m.reportMu.Unlock()
	if n != 0 {
		t.Fatalf("lastOversizedReport kept %d entries for a deleted bubble", n)
	}
}
