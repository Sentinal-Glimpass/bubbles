// Package runner launches and kills bubble sessions behind an interface, so the
// kernel never depends on real claude. LocalRunner/SSHRunner come in Plan 2.
package runner

import "github.com/Sentinal-Glimpass/bubbles/internal/addr"

// SpawnOpts configures a launched session.
type SpawnOpts struct {
	Persona    string
	Goal       string
	Model      string // claude --model alias ("sonnet"/"opus"/"fable"); "" => sonnet
	GrantSpawn bool   // root grants this bubble the spawn ability (depth 1: it can spawn, but its children cannot)
	SessionID  string // claude --session-id (new) / --resume target (restore)
	Resume     bool   // restored bubble: resume its conversation, no initial prompt
}

// DefaultModel is the model alias used when SpawnOpts.Model is empty. Aliases
// track the latest of each family, so "sonnet" is the current Sonnet.
const DefaultModel = "sonnet"

// Session is a running agent we can inject input into (message delivery).
type Session interface {
	Write(p []byte) (int, error)
	Close() error
	Alive() bool // false once the underlying process has exited (for lazy self-heal)
}

// Runner launches and kills sessions by address.
type Runner interface {
	Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error)
	Kill(a addr.Address) error
}
