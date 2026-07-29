package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture writes lines (one JSONL record per line) to a temp file and
// returns its path.
func writeFixture(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

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

func TestOutputTokensAreNotContext(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":999999}}}`,
	}
	p := writeFixture(t, lines)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextTokens != 5 {
		t.Fatalf("ContextTokens = %d, want 5 (output_tokens must be excluded)", got.ContextTokens)
	}
}

func TestHasCompactionDetectsTheMarker(t *testing.T) {
	withMarker := []string{
		`{"type":"summary","isCompactSummary":true}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`,
	}
	p := writeFixture(t, withMarker)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasCompaction {
		t.Fatal("HasCompaction = false, want true")
	}

	withoutMarker := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`,
	}
	p2 := writeFixture(t, withoutMarker)
	got2, err := Read(p2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.HasCompaction {
		t.Fatal("HasCompaction = true, want false")
	}
}

func TestTrailingNonAssistantEntriesDoNotHideUsage(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":7,"cache_creation_input_tokens":8,"cache_read_input_tokens":9,"output_tokens":100}}}`,
		`{"type":"user","message":{"content":"thanks"}}`,
	}
	p := writeFixture(t, lines)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextTokens != 24 { // 7+8+9
		t.Fatalf("ContextTokens = %d, want 24", got.ContextTokens)
	}
}

func TestNoUsageAnywhereReturnsErrNoUsage(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"content":"hello"}}`,
		`{"type":"user","message":{"content":"world"}}`,
	}
	p := writeFixture(t, lines)
	_, err := Read(p)
	if !errors.Is(err, ErrNoUsage) {
		t.Fatalf("err = %v, want ErrNoUsage", err)
	}
}

func TestOversizedLinesDoNotTruncateTheScan(t *testing.T) {
	// A single line over 1MB (past both the default 64KB scanner buffer and
	// the enlarged buffer's initial 1MB capacity, forcing it to grow) must
	// not abort the scan or hide the valid usage entry that follows it.
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", 2<<20) + `"}}`
	lines := []string{
		huge,
		`{"type":"assistant","message":{"usage":{"input_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5,"output_tokens":600}}}`,
	}
	p := writeFixture(t, lines)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextTokens != 12 { // 3+4+5
		t.Fatalf("ContextTokens = %d, want 12 (scan must survive the oversized line)", got.ContextTokens)
	}
}

func TestMalformedLinesAreSkippedNotFatal(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":1,"cache_read_input_tokens":1,"output_tokens":1}}}`,
		`{this is not valid json`,
		`{"type":"assistant","message":{"usage":{"input_tokens":2,"cache_creation_input_tokens":2,"cache_read_input_tokens":2,"output_tokens":2}}}`,
	}
	p := writeFixture(t, lines)
	got, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextTokens != 6 { // last valid entry: 2+2+2
		t.Fatalf("ContextTokens = %d, want 6", got.ContextTokens)
	}
}
