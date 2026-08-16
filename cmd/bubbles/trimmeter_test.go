package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// trimLog captures the manager's log sink so a test can prove a decision was
// announced. The daemon's real sink is stderr, which lands in
// .bubbles/daemon.log.
type trimLog struct {
	mu    sync.Mutex
	sb    strings.Builder
	lines int
}

func (l *trimLog) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sb.WriteString(fmt.Sprintf(format, a...))
	l.lines++
}

func (l *trimLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

// captureTrimLog redirects the manager's log sink and returns the capture.
func (f *pumpFixture) captureTrimLog() *trimLog {
	l := &trimLog{}
	f.m.logf = l.logf
	return l
}

func (f *pumpFixture) counters() (trimmed, archived, refused int64) {
	c := f.k.Cost.Snapshot()[f.a]
	return c.TranscriptsTrimmed, c.TranscriptBytesArchived, c.TrimsRefused
}

// TestTrimLogsAndMetersASuccessfulTrim: this incident took forensic
// archaeology because trimming logged nothing at all. One line, with every
// number needed to reconstruct what happened to the file.
func TestTrimLogsAndMetersASuccessfulTrim(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	log := f.captureTrimLog()
	path := f.writeIdentifiedTranscript(t, "sess-a", time.Hour)
	before := int64(len(readFile(t, path)))

	f.m.trimTranscripts()

	after := int64(len(readFile(t, path)))
	archive := int64(len(readFile(t, path+transcriptArchiveSuffix)))

	trimmed, archivedBytes, refused := f.counters()
	if trimmed != 1 {
		t.Errorf("TranscriptsTrimmed = %d, want 1", trimmed)
	}
	if archivedBytes != archive {
		t.Errorf("TranscriptBytesArchived = %d, want %d (the bytes actually written to the archive)", archivedBytes, archive)
	}
	if refused != 0 {
		t.Errorf("TrimsRefused = %d, want 0 on a successful trim", refused)
	}

	line := log.text()
	for _, want := range []string{
		"outcome=trimmed",
		path,
		string(f.a),
		"sess-a",
		fmt.Sprintf("before=%d", before),
		fmt.Sprintf("after=%d", after),
		fmt.Sprintf("archived=%d", archive),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("trim log is missing %q; got %q", want, line)
		}
	}
}

// TestIdentityRefusalIsNeverSilent: a refusal on identity grounds means the
// registry pointed at somebody else's file. That is the loudest thing this
// code can discover and it must never pass unrecorded.
func TestIdentityRefusalIsNeverSilent(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	log := f.captureTrimLog()
	f.writeIdentifiedTranscript(t, "someone-elses-session", time.Hour)

	f.m.trimTranscripts()

	if _, _, refused := f.counters(); refused != 1 {
		t.Errorf("TrimsRefused = %d, want 1", refused)
	}
	line := log.text()
	if !strings.Contains(line, "outcome=refused-identity") {
		t.Errorf("an identity refusal must say so; got %q", line)
	}
	if !strings.Contains(line, "someone-elses-session") {
		t.Errorf("the log must carry the session id RESOLVED FROM THE FILE, which is the surprising fact; got %q", line)
	}
}

// TestRecencyRefusalIsNeverSilent: refusing to rewrite a file that was being
// written to a minute ago is the gate doing its job, and is worth a line.
func TestRecencyRefusalIsNeverSilent(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	log := f.captureTrimLog()
	f.writeIdentifiedTranscript(t, "sess-a", time.Minute)

	f.m.trimTranscripts()

	if _, _, refused := f.counters(); refused != 1 {
		t.Errorf("TrimsRefused = %d, want 1", refused)
	}
	if line := log.text(); !strings.Contains(line, "outcome=refused-recent") {
		t.Errorf("a recency refusal must say so; got %q", line)
	}
}

// TestHotRefusalIsRecorded: the hotness skip is the commonest outcome of all,
// so it is recorded through the same throttle as its counter (see
// reportOversizedTranscript's precedent) — visible, but never once per sweep.
func TestHotRefusalIsRecorded(t *testing.T) {
	f := newPumpFixture(t)
	f.hot(t)
	b, _ := f.k.Reg.Get(f.a)
	log := f.captureTrimLog()
	f.writeIdentifiedTranscript(t, b.SessionID, time.Hour)

	f.m.trimTranscripts()
	if _, _, refused := f.counters(); refused != 1 {
		t.Errorf("TrimsRefused = %d, want 1", refused)
	}
	if line := log.text(); !strings.Contains(line, "outcome=refused-hot") {
		t.Errorf("a hot-bubble refusal must say so; got %q", line)
	}

	// Sweep runs every 2 minutes and a hot bubble stays hot. The LOG LINE must
	// not climb with the sweep cadence — that is the "OversizedTranscripts:
	// 214" lesson. The COUNTER must, because every suppression path in this
	// repo is metered on every suppression.
	f.m.trimTranscripts()
	f.m.trimTranscripts()
	if n := strings.Count(log.text(), "outcome=refused-hot"); n != 1 {
		t.Errorf("refused-hot logged %d times across 3 sweeps of one unchanged condition, want 1", n)
	}
	if _, _, refused := f.counters(); refused != 3 {
		t.Errorf("TrimsRefused = %d after 3 sweeps, want 3 — only the log line is rate-limited, never the meter", refused)
	}
}

// TestTrimLogThrottleCoversEverySteadyState: refused-identity and
// no-transcript are STICKY, not transient. This branch deliberately defers the
// SetSessionID persistence fix, so a bubble naming a session that does not
// exist hits the same outcome on every 2-minute sweep, forever. Unthrottled
// that is ~720 lines a day burying the very lines this work exists to surface.
func TestTrimLogThrottleCoversEverySteadyState(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-that-never-existed")
	log := f.captureTrimLog()

	f.m.trimTranscripts()
	f.m.trimTranscripts()
	f.m.trimTranscripts()

	if n := strings.Count(log.text(), "outcome=no-transcript"); n != 1 {
		t.Errorf("no-transcript logged %d times across 3 sweeps, want 1 — a sticky outcome must not repeat at sweep cadence", n)
	}
	if _, _, refused := f.counters(); refused != 3 {
		t.Errorf("TrimsRefused = %d, want 3 — the meter counts every attempt even when the line is suppressed", refused)
	}
}

// TestTrimLogAnnouncesAChangeOfStateImmediately is the other half of the
// throttle, and the reason its key carries the outcome: what is worth seeing is
// a condition CHANGING, and it must never wait out the previous condition's
// window.
func TestTrimLogAnnouncesAChangeOfStateImmediately(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	log := f.captureTrimLog()

	f.m.trimTranscripts() // no transcript on disk yet
	if !strings.Contains(log.text(), "outcome=no-transcript") {
		t.Fatal("setup: the first outcome was not recorded")
	}

	// Same bubble, same window, different outcome: somebody else's transcript
	// has appeared where this bubble's should be.
	f.writeIdentifiedTranscript(t, "someone-elses-session", time.Hour)
	f.m.trimTranscripts()

	if !strings.Contains(log.text(), "outcome=refused-identity") {
		t.Errorf("a CHANGE of outcome must be announced immediately, not swallowed by the previous outcome's window; got %q", log.text())
	}
}

// TestTrimFailureIsRecorded: an archive write that fails leaves the live file
// intact, which is silent by design — the counter and the line are the only
// way anyone finds out trimming has stopped working.
func TestTrimFailureIsRecorded(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	log := f.captureTrimLog()
	path := f.writeIdentifiedTranscript(t, "sess-a", time.Hour)
	if err := os.Mkdir(path+transcriptArchiveSuffix, 0o755); err != nil {
		t.Fatal(err)
	}

	f.m.trimTranscripts()

	if _, _, refused := f.counters(); refused != 1 {
		t.Errorf("TrimsRefused = %d, want 1 — a failed trim is a refused trim", refused)
	}
	if line := log.text(); !strings.Contains(line, "outcome=error") || !strings.Contains(line, "err=") {
		t.Errorf("a failed trim must log the error; got %q", line)
	}
}

// TestTrimLogsWhenThereIsNoTranscriptAtAll: four bubbles currently point at
// session ids with no file on disk. That is the exact staleness behind this
// incident, and it was invisible.
func TestTrimLogsWhenThereIsNoTranscriptAtAll(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-that-never-existed")
	log := f.captureTrimLog()

	f.m.trimTranscripts()

	if line := log.text(); !strings.Contains(line, "outcome=no-transcript") {
		t.Errorf("a registry pointing at a nonexistent transcript must be visible; got %q", line)
	}
}
