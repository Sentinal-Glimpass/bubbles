// Package runner launches and kills bubble sessions behind an interface, so the
// kernel never depends on real claude. LocalRunner/SSHRunner come in Plan 2.
package runner

import "github.com/Sentinal-Glimpass/bubbles/internal/addr"

// SpawnOpts configures a launched session.
type SpawnOpts struct {
	Persona   string
	Goal      string
	SessionID string // claude --session-id (new) / --resume target (restore)
	Resume    bool   // restored bubble: resume its conversation, no initial prompt
}

// Session is a running agent we can inject input into (message delivery).
type Session interface {
	Write(p []byte) (int, error)
	Close() error
}

// Runner launches and kills sessions by address.
type Runner interface {
	Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error)
	Kill(a addr.Address) error
}
