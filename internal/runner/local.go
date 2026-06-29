package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/creack/pty"
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// PTYSession is a Session backed by a PTY, exposing the master file so the TUI
// can hand the terminal directly to the user for dive-in.
type PTYSession interface {
	Session
	PTY() *os.File
}

// LocalRunner launches real claude sessions in PTYs on this machine.
type LocalRunner struct {
	Bin           string                    // default "claude"
	CitizenPrompt string                    // appended via --append-system-prompt
	MCPConfig     func(addr.Address) string // inline JSON for --mcp-config (nil = none)
	InterruptByte byte                      // sent before a delivered message (0 = none)

	mu       sync.Mutex
	sessions map[addr.Address]*ptySession
}

// NewLocal returns a LocalRunner with claude defaults (Esc as interrupt byte).
func NewLocal() *LocalRunner {
	return &LocalRunner{Bin: "claude", InterruptByte: 0x1b, sessions: map[addr.Address]*ptySession{}}
}

// Launch starts claude in a PTY in dir, seeded with the persona/goal.
func (r *LocalRunner) Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error) {
	var args []string
	// --mcp-config is variadic in claude (it consumes following values), so it
	// must NOT sit right before the positional prompt or the prompt gets eaten
	// as a second config path. We also write the config to a file rather than
	// passing inline JSON, which avoids quoting ambiguity.
	if r.MCPConfig != nil {
		// Write the config to a temp file (not the bubble's working dir, which
		// may be a real project folder we don't want to litter).
		cfgPath := filepath.Join(os.TempDir(), fmt.Sprintf("bubbles-mcp-%d-%s.json", os.Getpid(), a))
		if err := os.WriteFile(cfgPath, []byte(r.MCPConfig(a)), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--mcp-config", cfgPath)
	}
	if r.CitizenPrompt != "" {
		args = append(args, "--append-system-prompt", r.citizen(a))
	}
	args = append(args, "--permission-mode", "acceptEdits")
	args = append(args, initialPrompt(opts)) // positional prompt stays last

	cmd := exec.Command(r.Bin, args...)
	cmd.Dir = dir
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	s := &ptySession{cmd: cmd, ptmx: ptmx, interrupt: r.InterruptByte}
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = map[addr.Address]*ptySession{}
	}
	r.sessions[a] = s
	r.mu.Unlock()
	return s, nil
}

// Session returns the live session for a, or nil if none.
func (r *LocalRunner) Session(a addr.Address) Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[a]; ok {
		return s
	}
	return nil
}

// Kill terminates the session for a.
func (r *LocalRunner) Kill(a addr.Address) error {
	r.mu.Lock()
	s, ok := r.sessions[a]
	delete(r.sessions, a)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return s.Close()
}

// citizen embeds the bubble's address into the citizen system prompt.
func (r *LocalRunner) citizen(a addr.Address) string {
	return r.CitizenPrompt + "\nYou are bubble " + a.String() + ". Root (the human) is address 0."
}

func initialPrompt(o SpawnOpts) string {
	if o.Goal != "" {
		return o.Goal
	}
	return "You are the '" + o.Persona + "' bubble. Introduce yourself briefly, then await instructions."
}

// ptySession is a running claude process behind a PTY.
type ptySession struct {
	cmd       *exec.Cmd
	ptmx      *os.File
	interrupt byte
}

// Write delivers a message: optional interrupt byte, the payload, then CR
// (interrupt-style delivery; see Plan 2 §delivery).
func (s *ptySession) Write(p []byte) (int, error) {
	if s.interrupt != 0 {
		_, _ = s.ptmx.Write([]byte{s.interrupt})
	}
	n, err := s.ptmx.Write(p)
	if err != nil {
		return n, err
	}
	_, err = s.ptmx.Write([]byte{'\r'})
	return n, err
}

func (s *ptySession) Close() error {
	_ = s.ptmx.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
}

// PTY returns the master file for dive-in terminal handoff.
func (s *ptySession) PTY() *os.File { return s.ptmx }
