package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// The operator's fleet-wide model override (`bubbles --model <x>`).
//
// It lives in a FILE, not an env var, and is read at every launch. An env var
// would only reach a daemon started by that same invocation — `bubbles --model x`
// against an already-running fleet would silently do nothing. The file is read
// by the running daemon on each launch, so the override takes effect on the
// next launch of every bubble with no restart. Running bubbles keep their model
// until they next launch; nothing is killed to force it.

// modelOverridePath is set once in runApp so the sampler can show the override
// without threading baseDir through it.
var modelOverridePath string

func modelOverrideFile(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "model-override")
}

// readModelOverride returns the resolved override id, or "" when none is set.
func readModelOverride(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id, known := runner.ResolveModel(strings.TrimSpace(string(data)))
	if strings.TrimSpace(string(data)) == "" || !known {
		return "" // an empty or hand-edited bad file must never break launches
	}
	return id
}

// clearWords are the values that remove the override and return every bubble
// to its own model.
var clearWords = map[string]bool{"default": true, "off": true, "none": true, "clear": true, "auto": true}

// setModelOverride validates and writes (or clears) the override. It rejects a
// name it can't resolve: an unresolvable id would make every launch fail.
func setModelOverride(baseDir, name string) (string, error) {
	path := modelOverrideFile(baseDir)
	n := strings.TrimSpace(name)
	if clearWords[strings.ToLower(n)] {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return "", nil
	}
	id, known := runner.ResolveModel(n)
	if !known || n == "" {
		return "", fmt.Errorf("unknown model %q — use opus, opus-4-8, opus-5, opus-5-5, fable, a full claude-… id, or 'default' to clear", name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return id, os.WriteFile(path, []byte(id+"\n"), 0o644)
}

// modelFlagValue finds `--model x` or `--model=x` in args. ok is false when
// the flag is absent. It deliberately does not match --default-model.
func modelFlagValue(args []string) (string, bool) {
	for i, a := range args {
		if a == "--model" && i+1 < len(args) {
			return args[i+1], true
		}
		if v, found := strings.CutPrefix(a, "--model="); found {
			return v, true
		}
	}
	return "", false
}

// applyModelFlag handles `bubbles --model <x>` before the client attaches.
func applyModelFlag() {
	v, ok := modelFlagValue(os.Args[1:])
	if !ok {
		return
	}
	id, err := setModelOverride(defaultWorkspace(), v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bubbles:", err)
		os.Exit(2)
	}
	if id == "" {
		fmt.Println("bubbles: model override cleared — every bubble uses its own model again (on its next launch)")
		return
	}
	fmt.Printf("bubbles: model override → %s for EVERY bubble, on its next launch (running bubbles keep their model until then)\n", id)
	if os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1" {
		fmt.Println("bubbles: note — Bedrock is on, where --model is not passed (ANTHROPIC_MODEL drives); the override applies only to non-Bedrock launches")
	}
}
