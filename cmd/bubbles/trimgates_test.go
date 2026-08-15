package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeIdentifiedTranscript plants a trimmable transcript (50 stale lines, a
// compaction boundary, 5 live ones) at the bubble's convPath, with every entry
// carrying sessionID as claude records it, and backdates its mtime by age.
func (f *pumpFixture) writeIdentifiedTranscript(t *testing.T, sessionID string, age time.Duration) string {
	t.Helper()
	b, ok := f.k.Reg.Get(f.a)
	if !ok {
		t.Fatal("bubble vanished from registry")
	}
	p := convPath(f.home, f.dir, b.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := ""
	if sessionID != "" {
		sid = `"sessionId":"` + sessionID + `",`
	}
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString(`{` + sid + `"type":"user","uuid":"old` + strconv.Itoa(i) + `"}` + "\n")
	}
	sb.WriteString(`{` + sid + `"type":"system","uuid":"root","parentUuid":null}` + "\n")
	sb.WriteString(`{` + sid + `"type":"user","uuid":"sum","isCompactSummary":true}` + "\n")
	for i := 0; i < 5; i++ {
		sb.WriteString(`{` + sid + `"type":"assistant","uuid":"new` + strconv.Itoa(i) + `"}` + "\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTrimTrimsAQuiescentMatchingTranscript is the control for every refusal
// test below: with the gates satisfied, trimming still happens. Without it a
// gate that refused everything would pass the whole file.
func TestTrimTrimsAQuiescentMatchingTranscript(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	path := f.writeIdentifiedTranscript(t, "sess-a", time.Hour)
	before := readFile(t, path)

	f.m.trimTranscripts()

	if got := readFile(t, path); got == before {
		t.Fatal("a cold, quiescent transcript whose own sessionId matches the bubble must still be trimmed")
	}
	if _, err := os.Stat(path + transcriptArchiveSuffix); err != nil {
		t.Fatalf("the cut portion was not archived: %v", err)
	}
}

// TestTrimRefusesMismatchedSessionIdentity is the gate that answers the
// incident directly: the registry's SessionID does not reliably persist, so a
// bubble can point at another session's file. The file's own recorded identity
// is the fact that cannot go stale.
func TestTrimRefusesMismatchedSessionIdentity(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	path := f.writeIdentifiedTranscript(t, "someone-elses-session", time.Hour)
	before := readFile(t, path)

	f.m.trimTranscripts()

	if got := readFile(t, path); got != before {
		t.Fatalf("a transcript whose own sessionId is not the bubble's must NOT be rewritten (%d B -> %d B)", len(before), len(got))
	}
}

// TestTrimRefusesTranscriptWithNoSessionID: unknown identity is not permission.
// A file whose contents cannot vouch for whose conversation it is must never be
// rewritten on the strength of registry state alone.
func TestTrimRefusesTranscriptWithNoSessionID(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	path := f.writeIdentifiedTranscript(t, "", time.Hour)
	before := readFile(t, path)

	f.m.trimTranscripts()

	if got := readFile(t, path); got != before {
		t.Fatalf("a transcript carrying no sessionId at all must NOT be rewritten (%d B -> %d B)", len(before), len(got))
	}
}

// TestTrimRefusesRecentlyModifiedTranscript: a file being appended to must
// never be rewritten under its writer, whatever the kernel believes about
// hotness. IsHot is registry state, and registry state is exactly what proved
// unreliable; the mtime is the file's own testimony.
func TestTrimRefusesRecentlyModifiedTranscript(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "sess-a")
	path := f.writeIdentifiedTranscript(t, "sess-a", time.Minute)
	before := readFile(t, path)

	f.m.trimTranscripts()

	if got := readFile(t, path); got != before {
		t.Fatalf("a transcript modified 1 minute ago must NOT be rewritten (%d B -> %d B)", len(before), len(got))
	}

	// The other side of the gate: quiescent for an hour, and it is trimmed.
	f.writeIdentifiedTranscript(t, "sess-a", time.Hour)
	quiet := readFile(t, path)
	f.m.endSweep() // drop the memo so the second run reads the new file
	f.m.trimTranscripts()
	if got := readFile(t, path); got == quiet {
		t.Fatal("the recency gate must expire: a transcript untouched for an hour is trimmable")
	}
}

// TestTrimStillRefusesHotBubble: the new gates ADD to the hotness guard, they
// do not replace it. Claude appends to its .jsonl through a held fd, so
// rewriting a file it has open loses whatever it wrote in between.
func TestTrimStillRefusesHotBubble(t *testing.T) {
	f := newPumpFixture(t)
	f.hot(t) // EnsureAlive mints the session id
	b, _ := f.k.Reg.Get(f.a)
	path := f.writeIdentifiedTranscript(t, b.SessionID, time.Hour)
	before := readFile(t, path)

	f.m.trimTranscripts()

	if got := readFile(t, path); got != before {
		t.Fatalf("a HOT bubble's transcript must never be rewritten, even with matching identity and an old mtime (%d B -> %d B)", len(before), len(got))
	}
}
