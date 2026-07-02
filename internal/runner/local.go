package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	InterruptByte byte                      // optional byte before a delivered message (0 = none; default 0 so urgent messages are queued, not interrupting)
	AllowAll      *bool                     // shared toggle: true => --dangerously-skip-permissions
	MemMaxMB      int                       // default per-bubble RAM ceiling (0 = uncapped); each bubble runs in its own memory-capped cgroup so a runaway dies alone
	SessionFile   func(addr.Address) string // per-bubble file where a hook records the live session id (nil = no session-id tracking)

	mu       sync.Mutex
	sessions map[addr.Address]*ptySession
}

// writeSettings generates a claude --settings file wiring a hook (on several
// events) that records the live session id to SessionFile(a), so bubbles learns
// the CURRENT conversation id even after an in-session /resume. Returns "" if no
// SessionFile is configured.
func (r *LocalRunner) writeSettings(a addr.Address) string {
	if r.SessionFile == nil {
		return ""
	}
	sf := r.SessionFile(a)
	if sf == "" {
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	hook := []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": fmt.Sprintf("%q session-hook %q", self, sf)},
	}}}
	cfg := map[string]any{"hooks": map[string]any{
		"SessionStart":     hook, // fires on start / resume / clear
		"UserPromptSubmit": hook, // fires on every user turn
		"Stop":             hook, // fires at the end of every assistant turn
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	p := filepath.Join(os.TempDir(), fmt.Sprintf("bubbles-settings-%d-%s.json", os.Getpid(), a))
	if os.WriteFile(p, b, 0o600) != nil {
		return ""
	}
	return p
}

// wrapInScope runs a launch in its own transient cgroup scope
// (systemd-run --user --scope) so its resident memory is measured exactly
// (memory.current, including child processes) for the global RAM budget. A hard
// per-session MemoryMax is added ONLY when memMB>0 — by default a session is
// bounded by the global budget (LRU eviction), never OOM-killed at a per-session
// ceiling. When scopes are unsupported it returns the command unchanged.
func wrapInScope(bin string, args []string, memMB int, supported bool) (string, []string) {
	if !supported {
		return bin, args
	}
	wrap := []string{"--user", "--scope", "--quiet", "--collect", "-p", "MemorySwapMax=0"}
	if memMB > 0 {
		wrap = append(wrap, "-p", fmt.Sprintf("MemoryMax=%dM", memMB)) // optional hard cap
	}
	wrap = append(wrap, "--", bin)
	return "systemd-run", append(wrap, args...)
}

var (
	memCapOnce sync.Once
	memCapOK   bool
)

// memCapSupported reports (once, cached) whether this host can enforce a
// per-process RAM cap rootless: Linux + systemd --user + a delegated cgroup v2
// memory controller. Elsewhere (macOS, no systemd) launches run uncapped.
func memCapSupported() bool {
	memCapOnce.Do(func() {
		if runtime.GOOS != "linux" {
			return
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		probe := exec.Command("systemd-run", "--user", "--scope", "--quiet", "-p", "MemoryMax=64M", "--", "true")
		memCapOK = probe.Run() == nil
	})
	return memCapOK
}

// NewLocal returns a LocalRunner with claude defaults. InterruptByte is 0:
// urgent messages are typed in (queued for the next turn), never interrupting.
func NewLocal() *LocalRunner {
	return &LocalRunner{Bin: "claude", sessions: map[addr.Address]*ptySession{}}
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
	if sp := r.writeSettings(a); sp != "" {
		args = append(args, "--settings", sp) // hook that tracks the live session id
	}
	if r.CitizenPrompt != "" {
		args = append(args, "--append-system-prompt", r.citizen(a))
	}
	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	args = append(args, "--model", model)
	if r.AllowAll != nil && *r.AllowAll {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--permission-mode", "acceptEdits")
	}
	if opts.Resume {
		if opts.SessionID != "" {
			args = append(args, "--resume", opts.SessionID) // resume THIS bubble's session
		} else {
			args = append(args, "--continue") // fallback: most recent conversation in dir
		}
	} else {
		if opts.SessionID != "" {
			args = append(args, "--session-id", opts.SessionID) // tag the new session
		}
		args = append(args, initialPrompt(opts)) // positional prompt stays last
	}

	memMB := opts.MemMaxMB
	if memMB == 0 {
		memMB = r.MemMaxMB // fleet default
	}
	name, fullArgs := wrapInScope(r.Bin, args, memMB, memCapSupported())
	cmd := exec.Command(name, fullArgs...)
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
	wmu       sync.Mutex // serialize deliveries so text and Enter don't interleave
}

// Write types a message into the session, then submits it with Enter. The Enter
// is sent as a SEPARATE keypress after a short pause — otherwise claude treats
// the text+CR as one paste and the CR becomes a newline in the box instead of a
// submit (the message would just sit there unsent).
func (s *ptySession) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.interrupt != 0 {
		_, _ = s.ptmx.Write([]byte{s.interrupt})
	}
	n, err := s.ptmx.Write(p)
	if err != nil {
		return n, err
	}
	time.Sleep(150 * time.Millisecond) // let claude register the typed text first
	_, err = s.ptmx.Write([]byte{'\r'})
	return n, err
}

func (s *ptySession) Close() error {
	_ = s.ptmx.Close()
	if s.cmd.Process != nil {
		// pty.Start puts the child in its own session, so a negative-PID signal
		// kills the whole group — including claude under a systemd-run scope.
		if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = s.cmd.Process.Kill()
		}
	}
	return nil
}

// Alive probes the process on-demand (signal 0 — no watcher goroutine): true
// while claude is running, false once it has exited/crashed.
func (s *ptySession) Alive() bool {
	if s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	return s.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// MemBytes reports the session's resident memory. When the bubble runs in its
// own cgroup scope (the default, memory-capped launch), memory.current is exact
// and includes claude's child processes. Otherwise it falls back to the main
// process's RSS.
func (s *ptySession) MemBytes() uint64 {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	pid := s.cmd.Process.Pid
	if v, ok := cgroupMemCurrent(pid); ok {
		return v
	}
	return procRSS(pid)
}

// cgroupMemCurrent reads memory.current for pid's cgroup v2 (0::<path> in
// /proc/<pid>/cgroup).
func cgroupMemCurrent(pid int) (uint64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return 0, false
	}
	line := strings.TrimSpace(string(data))
	i := strings.Index(line, "::")
	if i < 0 {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", line[i+2:], "memory.current"))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// procRSS reads a process's resident set size from /proc/<pid>/statm (pages).
func procRSS(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}

// PTY returns the master file for dive-in terminal handoff.
func (s *ptySession) PTY() *os.File { return s.ptmx }
