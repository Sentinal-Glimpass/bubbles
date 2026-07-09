// Package runner launches and kills bubble sessions behind an interface, so the
// kernel never depends on real claude. LocalRunner/SSHRunner come in Plan 2.
package runner

import (
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// SpawnOpts configures a launched session.
type SpawnOpts struct {
	Persona    string // legacy label; Name is preferred
	Name       string // display name
	Goal       string // initial instruction / charter (the first prompt)
	Model      string // claude --model alias ("sonnet"/"opus"/"fable"); "" => sonnet
	GrantSpawn bool   // root grants this bubble the spawn ability (depth 1: it can spawn, but its children cannot)
	MemMaxMB   int    // per-bubble RAM ceiling in MiB (0 => runner default); the runaway dies alone at this cap
	SessionID  string // claude --session-id (new) / --resume target (restore)
	Resume     bool   // restored bubble: resume its conversation, no initial prompt
}

// DefaultModel is the --model passed when a bubble doesn't specify one. A plain
// alias (not empty, not a raw Bedrock id) so an unmodelled bubble on
// subscription gets a clean alias instead of falling through to a Bedrock-shaped
// ANTHROPIC_MODEL lingering in the shell. On Bedrock the env drives instead
// (--model is suppressed entirely — see local.go).
const DefaultModel = "sonnet"

// Session is a running agent we can inject input into (message delivery).
type Session interface {
	Write(p []byte) (int, error)
	Close() error
	Alive() bool             // false once the underlying process has exited (for lazy self-heal)
	MemBytes() uint64        // current resident memory of this session (0 = unknown); drives budget packing
	CPUTime() time.Duration  // cumulative CPU consumed by this session (for the usage dashboard)
	LastActivity() time.Time // when this session last produced output (idle detection); zero = unknown
	RecentOutput() string    // recent output text (for detecting a failed --resume)
	InputReady() bool        // true once claude is at a real input prompt (past any startup menu)
}

// Runner launches and kills sessions by address.
type Runner interface {
	Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error)
	Kill(a addr.Address) error
}
