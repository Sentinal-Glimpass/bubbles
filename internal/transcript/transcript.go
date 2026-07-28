// Package transcript is a pure reader for Claude Code conversation .jsonl
// files. It reports how large a conversation's context has grown and whether
// it has already been compacted, so callers can decide when to nudge a bubble
// toward /compact before its context gets expensive. No I/O beyond reading
// the given file, no clock, no side effects.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
)

// CompactMarker is the exact byte signature Claude Code writes for a
// compaction boundary. This package is the single authority for what a
// compaction looks like — everything that reads transcripts (including
// cmd/bubbles/health.go's trimTranscript) must use this constant rather than
// re-deriving the literal, so the two never drift apart.
var CompactMarker = []byte(`"isCompactSummary":true`)

// ContextNudgeTokens is the context-size (in tokens) threshold at which a
// bubble should be nudged toward compaction — it is also the threshold the
// resume "continue from summary / as-is" menu autopilot uses to pick
// "summary" instead of "as-is". Both meanings are the same fact ("this
// conversation has grown large enough to warrant summarizing") so they share
// one constant instead of two literals that could drift apart.
const ContextNudgeTokens = 500_000

// ContextForceTokens is the higher context-size threshold at which
// compaction is forced rather than merely suggested. It must stay greater
// than ContextNudgeTokens: a bubble should always get the polite nudge
// before it is ever force-compacted.
const ContextForceTokens = 800_000

// ErrNoUsage is returned when a transcript contains no entry carrying a usage
// object, so ContextTokens has no basis (e.g. a conversation with only user
// turns, or before the first assistant reply).
var ErrNoUsage = errors.New("transcript: no usage entry found")

// Stats summarizes a transcript file.
type Stats struct {
	// ContextTokens is input_tokens + cache_creation_input_tokens +
	// cache_read_input_tokens of the last entry that carried a usage object —
	// the full prompt size the model was billed for on its most recent turn.
	// output_tokens is deliberately excluded: it is what the model produced,
	// not what it had to hold in context.
	ContextTokens int64
	// HasCompaction is true if any line in the file carries the compaction
	// marker, meaning the conversation has already been summarized at least
	// once.
	HasCompaction bool
	// Entries is the count of lines that decoded successfully.
	Entries int
	// Bytes is the file size on disk.
	Bytes int64
}

// usageEntry is the minimal shape needed to find each line's usage numbers.
// Decoding is defensive: an entry with no usage, or a line that isn't even
// valid JSON, is simply skipped rather than treated as fatal — transcript
// shapes evolve and a single bad line must never take down the reader.
type usageEntry struct {
	Message struct {
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Read scans path line by line and computes Stats. It never mutates the file
// and never fails on individual malformed lines — only a file-level I/O error
// or scanner failure is returned as an error, alongside ErrNoUsage when no
// line ever carried usage.
func Read(path string) (Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stats{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Stats{}, err
	}

	var stats Stats
	stats.Bytes = info.Size()

	foundUsage := false
	scanner := bufio.NewScanner(f)
	// Transcript lines routinely exceed the default 64KB scanner buffer; a
	// silent bufio.ErrTooLong would truncate the scan and under-report
	// context, which is a correctness bug here, not just a robustness nicety.
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.Contains(line, CompactMarker) {
			stats.HasCompaction = true
		}

		var e usageEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // malformed line: skip, not fatal
		}
		stats.Entries++

		if e.Message.Usage != nil {
			u := e.Message.Usage
			stats.ContextTokens = u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
			foundUsage = true
		}
	}
	if err := scanner.Err(); err != nil {
		return Stats{}, err
	}

	if !foundUsage {
		return stats, ErrNoUsage
	}
	return stats, nil
}
