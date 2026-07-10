package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// This file holds the harness support pieces around tasks: the per-bubble brain
// folder the kernel seeds at spawn, and the fleet's shared decisions ledger.
// Shared state goes through the kernel (serialized), never a markdown file
// multiple bubbles write concurrently.

// BrainTemplate is the seed content of a fresh bubble's private BRAIN.md.
const BrainTemplate = `# Brain — %s

Your private memory. One fact/decision per section; keep it current, prune what
is stale. The workspace you run in may be shared with other bubbles — this
folder is yours alone.

## Charter

## Decisions (date — decision — why)

## Open threads
`

// SeedBrain provisions a bubble's private brain folder under BrainBase, keyed
// by ADDRESS (never by working dir — two bubbles sharing a workspace get two
// brains). Deterministic kernel operation, not an agent action: every spawned
// bubble is guaranteed the same skeleton. No-op when BrainBase is unset or the
// brain already exists (restored fleets keep their notes).
func (k *Kernel) SeedBrain(a addr.Address) string {
	if k.BrainBase == "" {
		return ""
	}
	dir := filepath.Join(k.BrainBase, a.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	brain := filepath.Join(dir, "BRAIN.md")
	if _, err := os.Stat(brain); os.IsNotExist(err) {
		_ = os.WriteFile(brain, []byte(fmt.Sprintf(BrainTemplate, a)), 0o644)
	}
	return dir
}

// LogDecision appends one entry to the fleet's shared decisions ledger — the
// steering-loop file. Serialized here so concurrent bubbles never interleave
// writes; agents call the log_decision tool instead of editing the file.
func (k *Kernel) LogDecision(by addr.Address, text string) error {
	if k.DecisionsPath == "" {
		return fmt.Errorf("kernel: no decisions ledger configured")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("kernel: empty decision")
	}
	name := by.String()
	if b, ok := k.Reg.Get(by); ok && b.Label() != "" {
		name += " (" + b.Label() + ")"
	}
	line := fmt.Sprintf("- %s — %s — %s\n", time.Now().Format("2006-01-02 15:04"), name, strings.ReplaceAll(text, "\n", " "))
	k.decisionsMu.Lock()
	defer k.decisionsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(k.DecisionsPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(k.DecisionsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}
