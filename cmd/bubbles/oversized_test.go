package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOversizedNoCompaction plants a COLD bubble's transcript that exceeds
// transcriptOversizedBytes and carries no compaction marker anywhere in it --
// the exact shape trimTranscript already declines to touch (latest < 0).
func (f *pumpFixture) writeOversizedNoCompaction(t *testing.T) string {
	t.Helper()
	b, ok := f.k.Reg.Get(f.a)
	if !ok {
		t.Fatal("bubble vanished from registry")
	}
	if b.SessionID == "" {
		t.Fatal("writeOversizedNoCompaction needs a session id")
	}
	p := convPath(f.home, f.dir, b.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","uuid":"a","parentUuid":null}` + "\n"
	var sb strings.Builder
	for int64(sb.Len()) <= transcriptOversizedBytes {
		sb.WriteString(line)
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTrimTranscriptsReportsNeverCompactedOversizedTranscript is the key test
// for this task: a COLD bubble whose transcript has never been compacted and
// exceeds the byte ceiling must be REPORTED (costmeter counter, at minimum),
// and its file must be left byte-identical -- trimTranscript is unsafe to run
// on a file with no compaction boundary, since every surviving line's
// parentUuid chain would be orphaned. This test exists so a future attempt to
// "finish the job" by truncating here fails loudly.
func TestTrimTranscriptsReportsNeverCompactedOversizedTranscript(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	path := f.writeOversizedNoCompaction(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	f.m.trimTranscripts()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("never-compacted oversized transcript must be left BYTE-IDENTICAL, but it was rewritten")
	}
	if got := f.k.Cost.Snapshot()[f.a].OversizedTranscripts; got != 1 {
		t.Fatalf("OversizedTranscripts = %d, want 1 -- the condition must be recorded", got)
	}
}
