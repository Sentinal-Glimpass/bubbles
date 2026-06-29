package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/tui"
)

const detachByte = 0x1c // Ctrl-\: instant detach fallback

// leaderByte is the in-bubble prefix key. Default Ctrl-A (like GNU screen) —
// it passes through VS Code/iTerm/Terminal over SSH, unlike Ctrl-Q which is
// intercepted as XON flow-control or a host-app shortcut. Override with the
// BUBBLES_LEADER env var, e.g. "ctrl-g" or "ctrl-b".
var leaderByte byte = 0x01

// parseLeader maps a name like "ctrl-a"/"c-g"/"ctrl-\" to its control byte.
func parseLeader(s string) byte {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"ctrl-", "ctrl+", "c-", "^"} {
		s = strings.TrimPrefix(s, p)
	}
	if len(s) == 1 {
		switch c := s[0]; {
		case c >= 'a' && c <= 'z':
			return c - 'a' + 1 // ctrl-a=0x01 .. ctrl-z=0x1a
		case c == '\\':
			return 0x1c
		case c == ']':
			return 0x1d
		}
	}
	return 0x01 // default Ctrl-A
}

// diveResult is the decision the leader state machine makes for one input byte.
type diveResult struct {
	forward  []byte       // bytes to write to claude
	switchTo addr.Address // non-empty => switch into this bubble
	fleet    bool         // true => return to the fleet view
}

// leaderState implements the leader prefix (default Ctrl-A, see leaderByte):
//
//	<leader> <leader> -> fleet
//	<leader> <digit>  -> if that slot is bound, switch to it; else bind the current bubble to it
//	Ctrl-\            -> instant fleet (no leader)
//
// everything else is forwarded to claude untouched (Esc included).
type leaderState struct{ armed bool }

func (s *leaderState) feed(b byte, current addr.Address, marks map[int]addr.Address) diveResult {
	if !s.armed {
		switch b {
		case leaderByte:
			s.armed = true
			return diveResult{}
		case detachByte:
			return diveResult{fleet: true}
		default:
			return diveResult{forward: []byte{b}}
		}
	}
	s.armed = false
	switch {
	case b == leaderByte: // leader leader -> fleet
		return diveResult{fleet: true}
	case b >= '0' && b <= '9':
		slot := int(b - '0')
		if dest, ok := marks[slot]; ok && dest != "" {
			if dest == current {
				return diveResult{} // already here
			}
			return diveResult{switchTo: dest}
		}
		marks[slot] = current // unbound slot -> bind the current bubble
		return diveResult{}
	default:
		return diveResult{forward: []byte{leaderByte, b}} // unknown: don't lose keys
	}
}

func runApp() {
	if v := os.Getenv("BUBBLES_LEADER"); v != "" {
		leaderByte = parseLeader(v)
	}
	baseDir := defaultWorkspace() // dir where `bubbles` was launched
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("bubbles-%d.sock", os.Getpid()))
	self, _ := os.Executable()

	lr := runner.NewLocal()
	lr.CitizenPrompt = citizenPrompt
	allowAll := true // default: launch bubbles with --dangerously-skip-permissions
	lr.AllowAll = &allowAll
	k := kernel.New(lr)
	lr.MCPConfig = func(a addr.Address) string {
		return mcpConfigJSON(self, sock, a, k.Caps.CanSpawn(a))
	}

	ln, err := ipc.Serve(sock, func(r ipc.Request) ipc.Reply { return handleIPC(k, r) })
	if err != nil {
		fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)

	// Quit/relaunch loop: the TUI quits when you dive in; we hand over the
	// terminal, then relaunch the fleet view.
	marks := restoreFleet(baseDir, k) // rehydrate a saved fleet (empty if none)
	m := tui.New(k)
	m.BaseDir = baseDir
	m.Marks = marks
	m.AllowAll = &allowAll
	for {
		p := tea.NewProgram(m, tea.WithAltScreen())
		k.Bus.Subscribe(addr.Root, func(msg bus.Message) {
			p.Send(tui.PingMsg{From: msg.From, Subject: msg.Subject})
		})
		final, err := p.Run()
		if err != nil {
			fatal(err)
		}
		_ = saveFleet(baseDir, k, marks) // persist fleet-view changes (spawn/introduce/marks)
		sel := final.(tui.Model).Selected
		if sel == "" {
			return // user quit
		}
		// Dive loop: keep switching bubble-to-bubble until we return to fleet.
		for sel != "" {
			sel = diveInto(lr, sel, marks)
		}
		_ = saveFleet(baseDir, k, marks) // persist anything spawned during the dive
		m = tui.New(k)                   // refresh rows, clear selection
		m.BaseDir = baseDir
		m.Marks = marks
		m.AllowAll = &allowAll
	}
}

// handleIPC maps a relayed tool call to a kernel operation. Identity is taken
// from the request's From/By (set by the helper to its own BUBBLE_ADDR).
func handleIPC(k *kernel.Kernel, r ipc.Request) ipc.Reply {
	from := addr.Address(r.From)
	switch r.Op {
	case "send":
		if err := k.Send(from, addr.Address(r.To), r.Subject, r.Body); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true}
	case "inbox":
		return ipc.Reply{OK: true, Messages: k.Inbox(from)}
	case "contacts":
		cs := k.Contacts(from)
		out := make([]string, len(cs))
		for i, c := range cs {
			label := c.String()
			if bub, ok := k.Reg.Get(c); ok && bub.Persona != "" {
				label += " (" + bub.Persona + ")" // attach the persona so peers have names/roles
			}
			out[i] = label
		}
		return ipc.Reply{OK: true, Contacts: out}
	case "spawn":
		dir := r.Dir
		if dir == "" {
			dir = filepath.Join(defaultWorkspace(), r.Persona) // downstream of launch dir
			_ = os.MkdirAll(dir, 0o755)
		}
		a, err := k.Spawn(from, r.Persona, dir, runner.SpawnOpts{Persona: r.Persona})
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: a.String()}
	default:
		return ipc.Reply{OK: false, Err: "unknown op: " + r.Op}
	}
}

// mcpConfigJSON builds the inline --mcp-config JSON pointing claude at our own
// binary in mcp-stdio mode, tagged with this bubble's address.
func mcpConfigJSON(exe, sock string, a addr.Address, spawnable bool) string {
	spawn := "0"
	if spawnable {
		spawn = "1"
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"bubbles": map[string]any{
				"type":    "stdio",
				"command": exe,
				"args":    []string{"mcp-stdio"},
				"env": map[string]string{
					"BUBBLE_ADDR":      a.String(),
					"BUBBLE_SOCK":      sock,
					"BUBBLE_SPAWNABLE": spawn,
				},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// diveInto hands the terminal to a bubble's PTY. It returns "" to go back to the
// fleet, or the address of another bubble to switch directly into (Ctrl-Q num).
func diveInto(lr *runner.LocalRunner, a addr.Address, marks map[int]addr.Address) addr.Address {
	sess := lr.Session(a)
	ps, ok := sess.(runner.PTYSession)
	if !ok || ps == nil {
		return ""
	}
	f := ps.PTY()

	// Size the bubble's PTY to fill the real terminal, and keep it synced on
	// window resize, so claude renders full-screen instead of in an 80x24 box.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			_ = pty.InheritSize(os.Stdin, f)
		}
	}()
	defer signal.Stop(winch)

	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	fmt.Print("\x1b[2J\x1b[H") // clear, so claude's full-screen redraw is clean
	// On the way out, disable any mouse reporting / bracketed paste claude turned on.
	defer fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\r\n")

	var detached atomic.Bool
	defer detached.Store(true)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if detached.Load() {
				return
			}
			if n > 0 {
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Now that we're copying the bubble's output, force claude (an Ink TUI) to
	// repaint — it only redraws on a size change, so re-entering an idle bubble
	// would otherwise look blank. Shrink a row, pause so Ink renders, then restore.
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
		smaller := *ws
		if smaller.Rows > 1 {
			smaller.Rows--
		}
		_ = pty.Setsize(f, &smaller)
		time.Sleep(60 * time.Millisecond)
		_ = pty.Setsize(f, ws)
	} else {
		_ = pty.InheritSize(os.Stdin, f)
	}

	// Input loop. Esc and everything else go straight to claude; the Ctrl-Q
	// leader (and Ctrl-\) are intercepted by the state machine.
	var ls leaderState
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return ""
		}
		if buf[0] == 0x1b { // escape sequence (arrows etc.) or a lone Esc
			seq := append([]byte{0x1b}, readEscapeRest()...)
			if isCtrlLeft(seq) {
				return "" // Ctrl-Left -> back to fleet
			}
			ls = leaderState{} // drop any half-typed leader
			f.Write(seq)       // forward arrows / lone Esc to claude
			continue
		}
		res := ls.feed(buf[0], a, marks)
		switch {
		case res.fleet:
			return ""
		case res.switchTo != "":
			return res.switchTo
		case len(res.forward) > 0:
			f.Write(res.forward)
		}
	}
}

// readEscapeRest grabs the rest of an escape sequence after an initial Esc, using
// a short poll so a lone Esc (interrupt) isn't held up. Returns the bytes that
// followed Esc (empty for a lone Esc).
func readEscapeRest() []byte {
	var out []byte
	timeout := 25 // ms to wait for a sequence to materialize after Esc
	for {
		fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return out
		}
		var b [32]byte
		rn, rerr := os.Stdin.Read(b[:])
		if rn > 0 {
			out = append(out, b[:rn]...)
		}
		if rerr != nil {
			return out
		}
		timeout = 5 // collect the rest of the burst, then stop
	}
}

// isCtrlLeft reports whether seq is a Ctrl-Left arrow (terminal-dependent forms).
func isCtrlLeft(seq []byte) bool {
	switch string(seq) {
	case "\x1b[1;5D", "\x1b[5D", "\x1bO5D":
		return true
	}
	return false
}

// defaultWorkspace is the directory where `bubbles` was launched; bubble folders
// are created downstream of it.
func defaultWorkspace() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bubbles:", err)
	os.Exit(1)
}
