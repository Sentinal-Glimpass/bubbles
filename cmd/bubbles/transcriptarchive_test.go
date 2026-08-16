package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeSyntheticTranscript plants a transcript of `old` pre-compaction lines, a
// null-parent root, a compaction summary, and `post` lines after it. It returns
// the whole file's bytes so a test can prove the trim is byte-exact.
func writeSyntheticTranscript(t *testing.T, path string, tag string, old, post int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < old; i++ {
		b.WriteString(`{"type":"user","uuid":"` + tag + `old` + strconv.Itoa(i) + `"}` + "\n")
	}
	b.WriteString(`{"type":"system","uuid":"` + tag + `root","parentUuid":null}` + "\n")
	b.WriteString(`{"type":"user","uuid":"` + tag + `sum","isCompactSummary":true}` + "\n")
	for i := 0; i < post; i++ {
		b.WriteString(`{"type":"assistant","uuid":"` + tag + `new` + strconv.Itoa(i) + `"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestTrimArchivesTheCutPortion is the heart of the fix: bubbles wanted a
// smaller live transcript and can have one without the cut history being
// destroyed. Archive + live must reconstruct the original byte for byte.
func TestTrimArchivesTheCutPortion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	original := writeSyntheticTranscript(t, path, "", 50, 5)

	if err := trimTranscript(path, 10); err != nil {
		t.Fatal(err)
	}

	live := readFile(t, path)
	archive := readFile(t, path+transcriptArchiveSuffix)
	if archive == "" {
		t.Fatal("the cut portion was destroyed instead of archived")
	}
	if archive+live != original {
		t.Fatalf("archive+live must reconstruct the original byte-for-byte:\narchive %d B, live %d B, original %d B", len(archive), len(live), len(original))
	}
	if !strings.Contains(archive, `"uuid":"old0"`) || !strings.Contains(archive, `"uuid":"old40"`) {
		t.Fatal("the archive must hold exactly the lines the live file lost")
	}
	if strings.Contains(archive, `"uuid":"old41"`) {
		t.Fatal("the archive must not hold lines the live file kept — the two must partition the original")
	}
}

// TestSecondTrimAppendsToArchive: a transcript is trimmed repeatedly over its
// life. If the second trim replaced the archive, the first cut's history would
// be destroyed on the next compaction — the original bug with one extra step.
func TestSecondTrimAppendsToArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	first := writeSyntheticTranscript(t, path, "a", 50, 5)

	if err := trimTranscript(path, 10); err != nil {
		t.Fatal(err)
	}

	// A second conversation segment lands on top, with its own compaction.
	var more strings.Builder
	for i := 0; i < 40; i++ {
		more.WriteString(`{"type":"user","uuid":"bold` + strconv.Itoa(i) + `"}` + "\n")
	}
	more.WriteString(`{"type":"user","uuid":"bsum","isCompactSummary":true}` + "\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(more.String()); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := trimTranscript(path, 10); err != nil {
		t.Fatal(err)
	}

	archive := readFile(t, path+transcriptArchiveSuffix)
	live := readFile(t, path)
	if !strings.Contains(archive, `"uuid":"aold0"`) {
		t.Fatal("the second trim replaced the archive instead of appending — the first cut's history is gone")
	}
	if !strings.Contains(archive, `"uuid":"bold0"`) {
		t.Fatal("the second cut was not archived")
	}
	if archive+live != first+more.String() {
		t.Fatalf("after two trims archive+live must still reconstruct everything ever written: archive %d B + live %d B != %d B", len(archive), len(live), len(first)+len(more.String()))
	}
}

// TestTrimLeavesLiveFileUntouchedWhenArchiveFails is THE test of this plan. A
// trim that half-succeeds is the bug: if the cut portion cannot be preserved,
// the live transcript must not be rewritten at all.
func TestTrimLeavesLiveFileUntouchedWhenArchiveFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	original := writeSyntheticTranscript(t, path, "", 50, 5)

	// Make the archive un-writable in a way no privilege level can bypass: a
	// directory already occupies its name.
	if err := os.Mkdir(path+transcriptArchiveSuffix, 0o755); err != nil {
		t.Fatal(err)
	}

	err := trimTranscript(path, 10)
	if err == nil {
		t.Fatal("a failed archive write must be reported, not swallowed")
	}
	if got := readFile(t, path); got != original {
		t.Fatalf("the live transcript was modified even though the archive write failed: %d B, want %d B", len(got), len(original))
	}
}

// TestArchiveIsNeverItselfTrimmed: the archive accumulates compaction markers
// by construction, so anything that mistook it for a transcript would cut it
// down — destroying the very history it exists to hold.
func TestArchiveIsNeverItselfTrimmed(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "s.jsonl"+transcriptArchiveSuffix)
	original := writeSyntheticTranscript(t, archive, "", 50, 5)

	err := trimTranscript(archive, 10)
	if err == nil {
		t.Fatal("trimming an archive must be refused loudly, not attempted")
	}
	if got := readFile(t, archive); got != original {
		t.Fatal("the archive was rewritten — it must never be trimmed")
	}
	if _, err := os.Stat(archive + transcriptArchiveSuffix); err == nil {
		t.Fatal("an archive of an archive was created")
	}
}

// TestSweepNeverTouchesArchives: the sweep resolves transcripts from session
// ids, so an archive must be invisible to it — never trimmed, never mistaken
// for a bubble's conversation.
func TestSweepNeverTouchesArchives(t *testing.T) {
	f := newPumpFixture(t)
	f.sessionID(t, "cold-session-id")
	b, _ := f.k.Reg.Get(f.a)
	path := convPath(f.home, f.dir, b.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSyntheticTranscript(t, path, "", 50, 5)
	archive := path + transcriptArchiveSuffix
	stale := writeSyntheticTranscript(t, archive, "arch", 50, 5)

	f.m.Sweep()

	if got := readFile(t, archive); !strings.HasPrefix(got, stale) {
		t.Fatal("the sweep rewrote an existing archive instead of only ever appending to it")
	}
}
