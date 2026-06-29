package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/tui"
)

// leaderByte (Ctrl-\) is the in-bubble leader prefix:
//
//	Ctrl-\ Ctrl-\   -> fleet
//	Ctrl-\ <digit>  -> jump to that slot if bound, else bind the current bubble
//
// Everything else (incl. Esc, arrows) goes straight to claude.
const leaderByte = 0x1c // Ctrl-\

// markAction handles a digit pressed after the Ctrl-Left leader: jump to a bound
// slot, or bind the current bubble to a free one. Returns the address to switch
// into, or "" to stay (already there / just bound).
func markAction(marks map[int]addr.Address, slot int, current addr.Address) addr.Address {
	if dest, ok := marks[slot]; ok && dest != "" {
		if dest == current {
			return ""
		}
		return dest
	}
	if marks != nil {
		marks[slot] = current
	}
	return ""
}

func runApp() {
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
		id, err := k.Send(from, addr.Address(r.To), r.Subject, r.Body, r.ReplyTo)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, ID: id}
	case "inbox":
		return ipc.Reply{OK: true, Messages: k.Inbox(from)}
	case "status":
		return ipc.Reply{OK: true, Messages: k.Status(from)}
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
	armed := false // true after a leader (Ctrl-\) press
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return ""
		}
		b := buf[0]
		if armed {
			armed = false
			switch {
			case b == leaderByte: // Ctrl-\ Ctrl-\ -> fleet
				return ""
			case b >= '0' && b <= '9':
				if dest := markAction(marks, int(b-'0'), a); dest != "" {
					return dest // switch into the bound bubble
				}
			default:
				f.Write([]byte{b}) // leader + other key: just send the key
			}
			continue
		}
		if b == leaderByte {
			armed = true
			continue
		}
		f.Write([]byte{b})
	}
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
