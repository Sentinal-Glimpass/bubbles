package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"

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

const detachByte = 0x1c // Ctrl-\ detaches from a dived-in bubble

func runApp() {
	baseDir := defaultWorkspace() // dir where `bubbles` was launched
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("bubbles-%d.sock", os.Getpid()))
	self, _ := os.Executable()

	lr := runner.NewLocal()
	lr.CitizenPrompt = citizenPrompt
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
	m := tui.New(k)
	m.BaseDir = baseDir
	for {
		p := tea.NewProgram(m, tea.WithAltScreen())
		k.Bus.Subscribe(addr.Root, func(msg bus.Message) {
			p.Send(tui.PingMsg{From: msg.From, Subject: msg.Subject})
		})
		final, err := p.Run()
		if err != nil {
			fatal(err)
		}
		sel := final.(tui.Model).Selected
		if sel == "" {
			return // user quit
		}
		diveInto(lr, sel)
		m = tui.New(k) // refresh rows, clear selection
		m.BaseDir = baseDir
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
	case "contacts":
		cs := k.Contacts(from)
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.String()
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

// diveInto hands the terminal to a bubble's PTY until the detach byte (Ctrl-\).
func diveInto(lr *runner.LocalRunner, a addr.Address) {
	sess := lr.Session(a)
	ps, ok := sess.(runner.PTYSession)
	if !ok || ps == nil {
		return
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
	winch <- syscall.SIGWINCH // trigger the initial resize now
	defer signal.Stop(winch)

	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	fmt.Print("\x1b[2J\x1b[H") // clear, so claude's full-screen redraw is clean

	var detached atomic.Bool
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

	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			break
		}
		if buf[0] == detachByte {
			break
		}
		f.Write(buf[:n])
	}
	detached.Store(true)
	// The inner claude session may have enabled mouse reporting / bracketed
	// paste; disable them so the fleet view (and the user's terminal) are clean.
	fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\r\n")
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
