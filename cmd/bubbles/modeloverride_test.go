package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelFlagValue(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"--model", "opus-5"}, "opus-5", true},
		{[]string{"--model=opus-4_8_1m"}, "opus-4_8_1m", true},
		{[]string{"--default-model", "x"}, "", false}, // must NOT match --default-model
		{[]string{"--mcp", "github"}, "", false},
		{[]string{"--model"}, "", false}, // dangling flag, no value
	} {
		got, ok := modelFlagValue(tc.args)
		if got != tc.want || ok != tc.ok {
			t.Errorf("modelFlagValue(%v) = (%q,%v), want (%q,%v)", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

// TestModelOverrideRoundTrip: set, read back resolved, clear.
func TestModelOverrideRoundTrip(t *testing.T) {
	base := t.TempDir()
	path := modelOverrideFile(base)

	if got := readModelOverride(path); got != "" {
		t.Fatalf("no file must mean no override, got %q", got)
	}
	id, err := setModelOverride(base, "opus-4_8_1m")
	if err != nil || id != "claude-opus-4-8[1m]" {
		t.Fatalf("set = (%q,%v), want claude-opus-4-8[1m]", id, err)
	}
	if got := readModelOverride(path); got != "claude-opus-4-8[1m]" {
		t.Fatalf("read back %q", got)
	}
	if _, err := setModelOverride(base, "default"); err != nil {
		t.Fatal(err)
	}
	if got := readModelOverride(path); got != "" {
		t.Fatalf("'default' must clear the override, still %q", got)
	}
}

// TestModelOverrideRejectsUnknown: an unresolvable override would make every
// launch fail, so it is refused and the previous state is left untouched.
func TestModelOverrideRejectsUnknown(t *testing.T) {
	base := t.TempDir()
	if _, err := setModelOverride(base, "opus-5"); err != nil {
		t.Fatal(err)
	}
	if _, err := setModelOverride(base, "gpt-9"); err == nil {
		t.Fatal("an unknown model must be rejected")
	}
	if got := readModelOverride(modelOverrideFile(base)); got != "claude-opus-5[1m]" {
		t.Fatalf("a rejected set must not disturb the existing override, got %q", got)
	}
}

// TestModelOverrideIgnoresBadFile: a hand-edited garbage file must not break
// every launch — it reads as "no override".
func TestModelOverrideIgnoresBadFile(t *testing.T) {
	base := t.TempDir()
	path := modelOverrideFile(base)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("gpt-9\n"), 0o644)
	if got := readModelOverride(path); got != "" {
		t.Fatalf("a bad override file must be ignored, got %q", got)
	}
}
