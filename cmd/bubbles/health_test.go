package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTrimTranscript: keeps from (latest compaction - keepBefore) onward, drops
// the summarized-away prefix, preserves bytes exactly, and no-ops safely.
func TestTrimTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")

	// Build a synthetic transcript: 50 old lines, a compaction root + summary,
	// then 5 post-compaction lines.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(`{"type":"user","uuid":"old` + strconv.Itoa(i) + `","parentUuid":"old` + strconv.Itoa(i-1) + `"}` + "\n")
	}
	b.WriteString(`{"type":"system","uuid":"root","parentUuid":null}` + "\n")           // line 50: the null-parent root
	b.WriteString(`{"type":"user","uuid":"sum","parentUuid":"root","isCompactSummary":true}` + "\n") // line 51: the marker
	for i := 0; i < 5; i++ {
		b.WriteString(`{"type":"assistant","uuid":"new` + strconv.Itoa(i) + `","parentUuid":"sum"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := trimTranscript(path, 10); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	s := string(out)

	// The marker (line 51) minus 10 = line 41 onward is kept; lines 0..40 dropped.
	if strings.Contains(s, `"uuid":"old0"`) || strings.Contains(s, `"uuid":"old40"`) {
		t.Fatalf("stale prefix not trimmed: %q", s[:120])
	}
	if !strings.Contains(s, `"uuid":"old41"`) { // 10 lines before the marker kept
		t.Fatal("the keepBefore buffer was not preserved")
	}
	if !strings.Contains(s, `"uuid":"root"`) || !strings.Contains(s, "isCompactSummary") || !strings.Contains(s, `"uuid":"new4"`) {
		t.Fatal("the active thread (root + summary + post-compaction) must survive intact")
	}

	// Idempotent-ish: running again keeps the (now single) compaction segment.
	before := s
	if err := trimTranscript(path, 10); err != nil {
		t.Fatal(err)
	}
	out2, _ := os.ReadFile(path)
	if len(out2) > len(before) {
		t.Fatal("re-trim should not grow the file")
	}

	// No compaction marker → no-op.
	plain := filepath.Join(dir, "plain.jsonl")
	os.WriteFile(plain, []byte(`{"a":1}`+"\n"+`{"a":2}`+"\n"), 0o644)
	if err := trimTranscript(plain, 10); err != nil {
		t.Fatal(err)
	}
	if d, _ := os.ReadFile(plain); string(d) != `{"a":1}`+"\n"+`{"a":2}`+"\n" {
		t.Fatal("a transcript with no compaction must be left untouched")
	}
}
