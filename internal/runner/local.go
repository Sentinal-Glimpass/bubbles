package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// PTYSession is a Session backed by a PTY, exposing the master file (for input +
// resize) and Attach (for output). Output is NOT read directly off PTY(): a
// single persistent drainer owns the read side, so the session never blocks on a
// full PTY buffer even when nobody is viewing it. Attach subscribes to that
// stream (replaying recent scrollback first, so dive-in isn't a black screen).
type PTYSession interface {
	Session
	PTY() *os.File
	Attach(w io.Writer) (detach func())
}

// scrollbackCap bounds the per-session output ring kept for dive-in replay. Big
// enough to hold a full claude repaint, small enough to be cheap per bubble.
const scrollbackCap = 256 * 1024

// LocalRunner launches real claude sessions in PTYs on this machine.
type LocalRunner struct {
	Bin           string                    // default "claude"
	CitizenPrompt string                    // appended via --append-system-prompt
	MCPConfig     func(addr.Address) string // inline JSON for --mcp-config (nil = none)
	InterruptByte byte                      // optional byte before a delivered message (0 = none; default 0 so urgent messages are queued, not interrupting)
	AllowAll      *bool                     // shared toggle: true => --dangerously-skip-permissions
	MemMaxMB      int                       // default per-bubble RAM ceiling (0 = uncapped); each bubble runs in its own memory-capped cgroup so a runaway dies alone
	SessionFile   func(addr.Address) string // per-bubble file where a hook records the live session id (nil = no session-id tracking)

	// StrictMCP passes --strict-mcp-config so a bubble sees ONLY our MCP server,
	// not the operator's inherited ones (github/playwright/gmail/…) — no context
	// bloat, no tools a bubble will never use. Configured, not compiled into
	// claude, so upgrades are unaffected.
	StrictMCP bool
	// DisallowedTools are built-in tools disabled per launch via --disallowed-tools.
	// Default: AskUserQuestion — its interactive in-chat menu blocks an autonomous
	// bubble waiting for a human that isn't there.
	DisallowedTools []string

	// ResumeSummaryThreshold drives the resume "continue from summary / as-is"
	// menu autopilot: on resume, if the conversation is >= this many tokens pick
	// "summary" (compact), else "as-is". 0 disables the autopilot.
	ResumeSummaryThreshold int

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
	return &LocalRunner{
		Bin:                    "claude",
		sessions:               map[addr.Address]*ptySession{},
		StrictMCP:              true,
		DisallowedTools:        []string{"AskUserQuestion"},
		ResumeSummaryThreshold: 500_000,
	}
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
		if r.StrictMCP {
			args = append(args, "--strict-mcp-config") // only OUR server; ignore inherited MCP servers
		}
	}
	if sp := r.writeSettings(a); sp != "" {
		args = append(args, "--settings", sp) // hook that tracks the live session id
	}
	if r.CitizenPrompt != "" {
		args = append(args, "--append-system-prompt", r.citizen(a))
	}
	// Disable interactive/unused built-in tools. Placed before --model so this
	// (variadic) flag stops consuming at the next --flag, never eating a prompt.
	if len(r.DisallowedTools) > 0 {
		args = append(args, "--disallowed-tools")
		args = append(args, r.DisallowedTools...)
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
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755) // a missing working dir would otherwise fail the launch (chdir)
	}
	cmd.Dir = dir
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	s := &ptySession{cmd: cmd, ptmx: ptmx, interrupt: r.InterruptByte, created: time.Now(), resume: opts.Resume, menuThreshold: r.ResumeSummaryThreshold}
	go s.drain()        // start reading immediately so claude never blocks on a full PTY buffer
	go s.readyWatcher() // boot detection + auto-answer the resume "summary/as-is" menu
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

// ptySession is a running claude process behind a PTY. A single drainer
// goroutine owns the read side of ptmx: it copies output into a bounded
// scrollback ring and, when a viewer is attached, forwards it live. This means
// the session is ALWAYS being read, so claude never blocks on a full PTY buffer
// (which would stall it mid-work and balloon its memory with unflushed output).
type ptySession struct {
	cmd       *exec.Cmd
	ptmx      *os.File
	interrupt byte
	wmu       sync.Mutex // serialize deliveries so text and Enter don't interleave

	rmu     sync.Mutex // guards ring + sub + lastOut
	ring    []byte     // recent output, capped at scrollbackCap (for dive-in replay)
	sub     io.Writer  // live viewer (dive-in), or nil when nobody is attached
	lastOut time.Time  // when the session last produced output (idle detection)
	created time.Time  // launch time (fallback activity stamp before any output)

	resume        bool        // this launch is a --resume (may show the summary menu)
	menuThreshold int         // token threshold for the summary-menu autopilot (0 = off)
	inputReady    atomic.Bool // true once claude is at a real input prompt (past any resume menu)
}

// InputReady reports whether claude is booted and at a real input prompt — i.e.
// past the resume "summary/as-is" menu — so a message typed in now won't land in
// that menu. Message delivery waits on this.
func (s *ptySession) InputReady() bool { return s.inputReady.Load() }

// resumeMenuOpt1 / resumeMenuOpt2 are stable labels from Claude Code's resume
// "continue" menu (verified in the 2.1.x bundle). Detecting both means it's up.
const (
	resumeMenuOpt1 = "Resume from summary"
	resumeMenuOpt2 = "Resume full session as-is"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
var tokenRE = regexp.MustCompile(`([\d][\d,]*(?:\.\d+)?)\s*([kKmM])?\s*tokens?`)

// idleFor reports how long since the session last produced output (0 if it never
// has). Quiet output means claude has finished booting/loading/repainting and is
// sitting at the prompt — the point at which a typed line will actually submit.
func (s *ptySession) idleFor() time.Duration {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.lastOut.IsZero() {
		return 0
	}
	return time.Since(s.lastOut)
}

// readyWatcher waits for claude to be genuinely at its input prompt before
// declaring the session input-ready. It (1) waits for first output, (2) on a
// resume, auto-answers the "Resume from summary / as-is" menu per the token
// threshold, then (3) waits for output to SETTLE — a resumed conversation can
// take several seconds to load and repaint, and typing a message before it's
// done leaves the text sitting unsubmitted. Only once output has been quiet do we
// mark it ready, so message delivery lands at a clean prompt. Bounded so we never
// hang if claude keeps streaming.
func (s *ptySession) readyWatcher() {
	bootDeadline := time.Now().Add(30 * time.Second)
	for s.RecentOutput() == "" {
		if !s.Alive() || time.Now().After(bootDeadline) {
			s.inputReady.Store(true)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	firstOut := time.Now()
	// resume: answer the summary menu if it appears (up to a few seconds).
	if s.resume && s.menuThreshold > 0 {
		menuDeadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(menuDeadline) {
			if !s.Alive() {
				s.inputReady.Store(true)
				return
			}
			out := s.RecentOutput()
			if strings.Contains(out, resumeMenuOpt1) && strings.Contains(out, resumeMenuOpt2) {
				s.answerResumeMenu(out)
				time.Sleep(500 * time.Millisecond) // let it start repainting the real prompt
				break
			}
			if time.Since(firstOut) > 6*time.Second { // no menu; move on to the settle-wait
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	// Wait for output to go quiet (claude finished loading/repainting and is at the
	// prompt). Fallback deadline so we never hang if it keeps producing output.
	settleDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(settleDeadline) {
		if !s.Alive() {
			break
		}
		if s.idleFor() >= 1200*time.Millisecond {
			break // settled -> at the prompt
		}
		time.Sleep(150 * time.Millisecond)
	}
	s.inputReady.Store(true)
}

// answerResumeMenu selects an option in the resume menu. The menu is a select
// list whose first (highlighted) item is "Resume from summary"; the second is
// "Resume full session as-is". Enter picks summary; Down then Enter picks as-is.
func (s *ptySession) answerResumeMenu(out string) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if parseTokenCount(out) >= s.menuThreshold {
		_, _ = s.ptmx.Write([]byte("\r")) // summary/compact (default option)
		return
	}
	_, _ = s.ptmx.Write([]byte("\x1b[B")) // down -> "Resume full session as-is"
	time.Sleep(150 * time.Millisecond)
	_, _ = s.ptmx.Write([]byte("\r"))
}

// parseTokenCount pulls a token count out of the menu text (e.g. "512,340 tokens",
// "1.2M tokens"). Returns 0 if none found — which, being below any positive
// threshold, defaults to the non-lossy "as-is" choice.
func parseTokenCount(s string) int {
	clean := ansiRE.ReplaceAllString(s, "")
	best := 0
	for _, m := range tokenRE.FindAllStringSubmatch(clean, -1) {
		num := strings.ReplaceAll(m[1], ",", "")
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "k":
			f *= 1e3
		case "m":
			f *= 1e6
		}
		if int(f) > best {
			best = int(f)
		}
	}
	return best
}

// drain is the persistent reader: it never stops until the PTY closes, so the
// process is always drained. Output is appended to the scrollback ring and, if a
// viewer is attached, streamed to it.
func (s *ptySession) drain() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.rmu.Lock()
			s.ring = appendRing(s.ring, buf[:n])
			s.lastOut = time.Now() // output = the session is doing work (not idle)
			if s.sub != nil {
				_, _ = s.sub.Write(buf[:n])
			}
			s.rmu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// Attach subscribes w to the live output stream, first replaying the recent
// scrollback so the viewer sees claude's current frame immediately instead of a
// black screen. Returns a detach func to stop forwarding. Only one viewer at a
// time (dive-in is single-terminal); a second Attach replaces the first.
func (s *ptySession) Attach(w io.Writer) (detach func()) {
	s.rmu.Lock()
	if len(s.ring) > 0 {
		_, _ = w.Write(s.ring) // scrollback replay -> instant paint
	}
	s.sub = w
	s.rmu.Unlock()
	return func() {
		s.rmu.Lock()
		if s.sub == w {
			s.sub = nil
		}
		s.rmu.Unlock()
	}
}

// appendRing appends p to ring, keeping only the last scrollbackCap bytes.
func appendRing(ring, p []byte) []byte {
	ring = append(ring, p...)
	if len(ring) > scrollbackCap {
		trimmed := make([]byte, scrollbackCap)
		copy(trimmed, ring[len(ring)-scrollbackCap:])
		ring = trimmed
	}
	return ring
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

// LastActivity reports when the session last produced output — the signal for
// idle page-out. Falls back to launch time before any output has arrived.
func (s *ptySession) LastActivity() time.Time {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.lastOut.IsZero() {
		return s.created
	}
	return s.lastOut
}

// RecentOutput returns the session's recent output (the scrollback ring), used
// to detect a --resume that failed with "No conversation found".
func (s *ptySession) RecentOutput() string {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return string(s.ring)
}

// CPUTime reports cumulative CPU consumed by the session. When it runs in its
// own cgroup scope (the default) cpu.stat/usage_usec is exact and includes child
// processes; otherwise it falls back to the main process's utime+stime.
func (s *ptySession) CPUTime() time.Duration {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	pid := s.cmd.Process.Pid
	if d, ok := cgroupCPUUsage(pid); ok {
		return d
	}
	return procCPU(pid)
}

// cgroupCPUUsage reads usage_usec from cpu.stat for pid's cgroup v2.
func cgroupCPUUsage(pid int) (time.Duration, bool) {
	line, ok := cgroupPath(pid)
	if !ok {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", line, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "usage_usec ") {
			v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(ln, "usage_usec ")), 10, 64)
			if err != nil {
				return 0, false
			}
			return time.Duration(v) * time.Microsecond, true
		}
	}
	return 0, false
}

// procCPU reads a process's own CPU time (utime+stime) from /proc/<pid>/stat.
func procCPU(pid int) time.Duration {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// fields 14 (utime) and 15 (stime) are in clock ticks; skip past comm in parens
	s := string(b)
	if i := strings.LastIndex(s, ") "); i >= 0 {
		s = s[i+2:]
	}
	f := strings.Fields(s)
	if len(f) < 13 {
		return 0
	}
	utime, _ := strconv.ParseUint(f[11], 10, 64)
	stime, _ := strconv.ParseUint(f[12], 10, 64)
	hz := uint64(100) // USER_HZ is 100 on Linux/amd64
	return time.Duration((utime+stime)*1e9/hz) * time.Nanosecond
}

// cgroupPath returns pid's cgroup v2 relative path (from 0::<path>).
func cgroupPath(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(data))
	i := strings.Index(line, "::")
	if i < 0 {
		return "", false
	}
	return line[i+2:], true
}

// cgroupMemCurrent reads memory.current for pid's cgroup v2 (0::<path> in
// /proc/<pid>/cgroup).
func cgroupMemCurrent(pid int) (uint64, bool) {
	line, ok := cgroupPath(pid)
	if !ok {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", line, "memory.current"))
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
