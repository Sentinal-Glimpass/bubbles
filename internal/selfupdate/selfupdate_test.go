package selfupdate

import (
	"testing"
	"time"
)

func TestPseudoSHA(t *testing.T) {
	cases := map[string]string{
		"v0.3.2-0.20260915120000-abcdef123456": "abcdef123456",
		"v0.3.1":                               "", // clean tag: no sha
		"(devel)":                              "",
		"":                                     "",
		"v1.0.0-rc1":                           "", // "rc1" isn't hex
	}
	for in, want := range cases {
		if got := pseudoSHA(in); got != want {
			t.Errorf("pseudoSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSameCommit is the crux of update detection: a short sha (from a
// pseudo-version) must match the full sha (from the GitHub API) for the SAME
// commit, and must NOT match a different one.
func TestSameCommit(t *testing.T) {
	full := "abcdef1234567890abcdef1234567890abcdef12"
	short := "abcdef123456" // 12-char prefix, as embedded in a pseudo-version
	if !SameCommit(short, full) {
		t.Fatal("a short sha must match its full sha (prefix)")
	}
	if !SameCommit(full, short) {
		t.Fatal("SameCommit must be order-independent")
	}
	if !SameCommit("ABCDEF123456", full) {
		t.Fatal("comparison must be case-insensitive")
	}
	if SameCommit("abcdef123456", "fedcba654321"+full[12:]) {
		t.Fatal("different commits must not match")
	}
	if SameCommit("abc", full) {
		t.Fatal("an under-7-char (unknown) revision must never match")
	}
	if SameCommit("", "") {
		t.Fatal("two empty revisions are unknown, not equal")
	}
}

// TestExtractSHA reads the commit back out of `bubbles version` output — the
// mechanism by which the daemon learns the on-disk binary's revision.
func TestExtractSHA(t *testing.T) {
	cases := map[string]string{
		"main-abcdef123456":         "abcdef123456",
		"v0.3.1 (abcdef123456)":     "abcdef123456",
		"dev":                       "",
		"v0.3.1":                    "", // no sha, and the tag digits are too short/dotted
		"abcdef1234567890abcdef12":  "abcdef1234567890abcdef12",
	}
	for in, want := range cases {
		if got := ExtractSHA(in); got != want {
			t.Errorf("ExtractSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDueForCheck(t *testing.T) {
	now := time.Now()
	if !DueForCheck(time.Time{}, now, time.Hour) {
		t.Fatal("a never-checked stamp must be due")
	}
	if DueForCheck(now.Add(-30*time.Minute), now, time.Hour) {
		t.Fatal("30m ago with a 1h interval must NOT be due")
	}
	if !DueForCheck(now.Add(-2*time.Hour), now, time.Hour) {
		t.Fatal("2h ago with a 1h interval must be due")
	}
}
