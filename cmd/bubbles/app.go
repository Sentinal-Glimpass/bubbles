package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
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
	if marks != nil { // bind: one slot per bubble (clear it from any other slot)
		for s, x := range marks {
			if x == current && s != slot {
				delete(marks, s)
			}
		}
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
	lr.MemMaxMB = 0 // no per-session hard cap (it was killing legit busy sessions); each runs in its own cgroup scope for measurement, bounded only by the global budget below
	k := kernel.New(lr)
	k.MemBudget = 45 << 30       // 45 GB total: sessions are packed by ACTUAL RAM; the coldest page out when the sum exceeds this
	k.IdleTimeout = 30 * time.Minute // page out sessions silent (no output) this long; they resume on next use
	lr.MCPConfig = func(a addr.Address) string {
		return mcpConfigJSON(self, sock, a, k.Caps.CanSpawn(a))
	}
	// Session-id tracking: a hook records each session's live id to this file; the
	// kernel reads it so an in-session /resume is what resumes next time.
	lr.SessionFile = func(a addr.Address) string { return sessionFile(baseDir, a) }
	k.CurrentSessionID = func(a addr.Address) string { return readSessionFile(baseDir, a) }
	go func() { // periodic memory sweep: catch sessions that grow past the budget over time
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			k.EnforceBudget()
		}
	}()
	go func() { // periodic idle sweep: page out sessions that have gone quiet
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			k.EvictIdle()
		}
	}()
	go func() { // periodic inbox drain: deliver pooled (non-urgent) messages so none go unanswered
		t := time.NewTicker(time.Duration(messagePollMinutes()) * time.Minute)
		defer t.Stop()
		for range t.C {
			k.DrainInboxes()
		}
	}()

	// Resource sampler: feeds the dashboard's top-right panel. It sends to
	// whichever TUI program is currently running (nil while diving into a bubble).
	var curProg atomic.Pointer[tea.Program]
	go runSampler(k, &curProg)

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
	// Persist promptly whenever the fleet changes (incl. agent-driven spawn/edit/
	// delete over IPC), so nothing is lost if the daemon dies before the next dive.
	m.OnPersist = func() { k.SyncSessionIDs(); _ = saveFleet(baseDir, k, marks) }
	for {
		p := tea.NewProgram(m, tea.WithAltScreen())
		curProg.Store(p)
		k.Bus.Subscribe(addr.Root, func(msg bus.Message) {
			p.Send(tui.PingMsg{From: msg.From, Subject: msg.Subject})
		})
		final, err := p.Run()
		curProg.Store(nil)
		if err != nil {
			fatal(err)
		}
		k.SyncSessionIDs()               // capture any in-session /resume before persisting
		_ = saveFleet(baseDir, k, marks) // persist fleet-view changes (spawn/introduce/marks)
		prev := final.(tui.Model)        // carries view state (expanded nodes, cursor, marks)
		sel := prev.Selected
		if sel == "" { // q / ctrl-c
			if hostedMode {
				fmt.Print(detachSentinel) // tell the client to detach; the fleet keeps running
				m = prev.Refreshed()      // restart the view for the next attach, keeping its state
				continue
			}
			return // --local: actually quit
		}
		// Dive loop: keep switching bubble-to-bubble until we return to fleet.
		flash := ""
		for sel != "" {
			next, launched := diveInto(k, sel, marks)
			if !launched {
				flash = "⚠ couldn't launch " + sel.String() + " — its folder may be missing; check .bubbles/daemon.log"
			}
			sel = next
		}
		k.SyncSessionIDs()               // capture any in-session /resume before persisting
		_ = saveFleet(baseDir, k, marks) // persist anything spawned during the dive
		m = prev.Refreshed()             // back to fleet: same expand state + cursor where we left
		m.Flash = flash
	}
}

// runSampler polls per-session resource use every couple of seconds, turns the
// cumulative CPU counters into a live percentage (delta over wall time), ranks
// the busiest bubbles, and pushes a snapshot to the current TUI program.
func runSampler(k *kernel.Kernel, curProg *atomic.Pointer[tea.Program]) {
	type prevSample struct {
		cpu time.Duration
		at  time.Time
	}
	prev := map[addr.Address]prevSample{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		prog := curProg.Load()
		samples := k.SampleUsage()
		now := time.Now()
		var totalMem uint64
		var totalCPU float64
		seen := map[addr.Address]bool{}
		rows := make([]tui.UsageRow, 0, len(samples))
		for _, s := range samples {
			seen[s.Addr] = true
			totalMem += s.Mem
			pct := 0.0
			if p, ok := prev[s.Addr]; ok {
				if dt := now.Sub(p.at).Seconds(); dt > 0 {
					pct = (s.CPU - p.cpu).Seconds() / dt * 100
				}
			}
			prev[s.Addr] = prevSample{s.CPU, now}
			totalCPU += pct
			rows = append(rows, tui.UsageRow{Name: s.Name, Mem: s.Mem, CPU: pct})
		}
		for a := range prev { // forget dead sessions so the map doesn't grow
			if !seen[a] {
				delete(prev, a)
			}
		}
		if prog == nil {
			continue // nobody viewing (mid-dive): still refresh prev, just don't send
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].CPU != rows[j].CPU {
				return rows[i].CPU > rows[j].CPU
			}
			return rows[i].Mem > rows[j].Mem
		})
		if len(rows) > 5 {
			rows = rows[:5]
		}
		prog.Send(tui.UsageMsg{TotalMem: totalMem, TotalCPU: totalCPU, Hot: len(samples), Top: rows})
	}
}

// handleIPC maps a relayed tool call to a kernel operation. Identity is taken
// from the request's From/By (set by the helper to its own BUBBLE_ADDR).
func handleIPC(k *kernel.Kernel, r ipc.Request) ipc.Reply {
	from := addr.Address(r.From)
	switch r.Op {
	case "send":
		id, err := k.Send(from, addr.Address(r.To), r.Subject, r.Body, r.ReplyTo, r.Urgent)
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
			if bub, ok := k.Reg.Get(c); ok && bub.Label() != "" {
				label += " (" + bub.Label() + ")" // attach the name so peers have names/roles
			}
			out[i] = label
		}
		return ipc.Reply{OK: true, Contacts: out}
	case "spawn":
		dir := r.Dir
		if dir == "" {
			dir = filepath.Join(defaultWorkspace(), r.Name) // downstream of launch dir
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(defaultWorkspace(), dir) // resolve a relative dir against the workspace
		}
		// ALWAYS ensure the dir exists — a spawn whose folder was never created
		// would fail to launch (claude can't chdir), which shows up as a bubble
		// that silently won't open on Enter.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return ipc.Reply{OK: false, Err: "cannot create dir " + dir + ": " + err.Error()}
		}
		a, err := k.Spawn(from, "", dir, runner.SpawnOpts{Name: r.Name, Goal: r.Description, Model: r.Model})
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: a.String()}
	case "edit":
		if err := k.EditBy(from, addr.Address(r.Addr), r.Name, r.Model, r.Description); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: r.Addr}
	case "delete":
		victims, err := k.DeleteBy(from, addr.Address(r.Addr))
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: r.Addr, ID: len(victims)} // ID = removed count
	case "forget":
		if err := k.Forget(from, addr.Address(r.Addr)); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: r.Addr}
	default:
		return ipc.Reply{OK: false, Err: "unknown op: " + r.Op}
	}
}

// sessionFile is where a bubble's session hook records its live conversation id.
func sessionFile(baseDir string, a addr.Address) string {
	return filepath.Join(baseDir, ".bubbles", "sessions", a.String())
}

// readSessionFile returns the live session id recorded for a (empty if none).
func readSessionFile(baseDir string, a addr.Address) string {
	data, err := os.ReadFile(sessionFile(baseDir, a))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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
// EnsureAlive heals a dead session first, so diving into a crashed bubble (or
// one whose resume id vanished) transparently relaunches it.
func diveInto(k *kernel.Kernel, a addr.Address, marks map[int]addr.Address) (next addr.Address, launched bool) {
	sess := k.EnsureAlive(a)
	ps, ok := sess.(runner.PTYSession)
	if !ok || ps == nil {
		return "", false // launch failed (bad dir/args, crashed on boot) — caller surfaces it
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
	fmt.Print("\x1b[?25h\x1b[2J\x1b[H") // show cursor (Bubbletea's alt-screen hides it) + clear for claude's redraw
	// On the way out, disable any mouse reporting / bracketed paste claude turned on.
	defer fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\r\n")

	// Subscribe to the session's persistent output stream (replays recent
	// scrollback first, so we see claude's current frame, not a black screen).
	// We do NOT read f directly — the drainer owns the read side so claude is
	// always drained and never stalls, viewed or not.
	detach := ps.Attach(os.Stdout)
	defer detach()

	// Force claude (an Ink TUI) to repaint — it only redraws on a size change, so
	// re-entering an idle bubble would otherwise show a stale frame. Shrink a row,
	// pause so Ink renders, then restore.
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
			return "", true
		}
		b := buf[0]
		if armed {
			armed = false
			switch {
			case b == leaderByte: // Ctrl-\ Ctrl-\ -> fleet
				return "", true
			case b >= '0' && b <= '9':
				if dest := markAction(marks, int(b-'0'), a); dest != "" {
					return dest, true // switch into the bound bubble
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
