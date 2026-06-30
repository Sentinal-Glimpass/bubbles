package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/control"
	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// controlSock is the daemon's control socket for a workspace (one daemon per dir).
func controlSock(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "control.sock")
}

// daemon is the long-lived process that owns the kernel and every claude PTY, so
// the fleet survives the TUI closing.
type daemon struct {
	k       *kernel.Kernel
	baseDir string
	stop    chan struct{}

	mu    sync.Mutex
	marks map[int]addr.Address
}

// runDaemon sets up the kernel, MCP socket, and control server, restores the
// fleet, then runs until told to stop (control "stop" op or SIGTERM/SIGINT).
func runDaemon() {
	baseDir := defaultWorkspace()
	_ = os.MkdirAll(filepath.Join(baseDir, ".bubbles"), 0o755)
	mcpSock := filepath.Join(os.TempDir(), fmt.Sprintf("bubbles-daemon-%d.sock", os.Getpid()))
	self, _ := os.Executable()

	lr := runner.NewLocal()
	lr.CitizenPrompt = citizenPrompt
	allowAll := true
	lr.AllowAll = &allowAll
	k := kernel.New(lr)
	lr.MCPConfig = func(a addr.Address) string { return mcpConfigJSON(self, mcpSock, a, k.Caps.CanSpawn(a)) }

	ipcLn, err := ipc.Serve(mcpSock, func(r ipc.Request) ipc.Reply { return handleIPC(k, r) })
	if err != nil {
		fatal(err)
	}
	defer ipcLn.Close()
	defer os.Remove(mcpSock)

	marks := restoreFleet(baseDir, k)
	d := &daemon{k: k, baseDir: baseDir, marks: marks, stop: make(chan struct{})}

	ctl := controlSock(baseDir)
	_ = os.Remove(ctl) // clear any stale socket
	ctlLn, err := control.Serve(ctl, d.handle)
	if err != nil {
		fatal(err)
	}
	defer ctlLn.Close()
	defer os.Remove(ctl)

	go func() { // periodic durability save
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.save()
			case <-d.stop:
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-d.stop:
	case <-sig:
	}
	d.save()
}

func (d *daemon) save() {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = saveFleet(d.baseDir, d.k, d.marks)
}

func (d *daemon) snapshot() *control.Snapshot {
	var bubbles []control.BubbleInfo
	for _, b := range d.k.Reg.All() {
		bubbles = append(bubbles, control.BubbleInfo{
			Addr: b.Addr.String(), Persona: b.Persona, Parent: b.Parent.String(),
			Status: string(b.Status), Unread: d.k.Store.UnreadCount(b.Addr),
		})
	}
	var grs []control.GroupInfo
	for _, g := range d.k.Groups.All() {
		ms := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			ms = append(ms, m.String())
		}
		grs = append(grs, control.GroupInfo{Name: g.Name, Members: ms, Session: g.Session.String()})
	}
	mk := map[string]string{}
	for slot, a := range d.marks {
		mk[strconv.Itoa(slot)] = a.String()
	}
	return &control.Snapshot{Bubbles: bubbles, Groups: grs, Marks: mk}
}

func (d *daemon) handle(r control.Request) control.Reply {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Op {
	case "snapshot":
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "spawn":
		a, err := d.k.SpawnUnder(addr.Root, addr.Address(r.Parent), r.Persona, r.Dir, runner.SpawnOpts{Persona: r.Persona})
		if err != nil {
			return control.Reply{OK: false, Err: err.Error()}
		}
		return control.Reply{OK: true, Addr: a.String(), Snapshot: d.snapshot()}
	case "startRoot":
		_ = d.k.StartRoot(r.Dir)
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "introduce":
		ms := toAddrs(r.Members)
		for i := 0; i < len(ms); i++ {
			for j := i + 1; j < len(ms); j++ {
				_ = d.k.Introduce(addr.Root, ms[i], ms[j])
			}
		}
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "createGroup":
		d.k.CreateGroup(r.Name, toAddrs(r.Members), r.IntroduceAll)
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "attachGroupSession":
		_, _ = d.k.AttachGroupSession(r.Name, r.Dir, runner.SpawnOpts{Persona: "#" + r.Name})
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "deleteGroup":
		d.k.DeleteGroup(r.Name)
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "setMark":
		if d.marks == nil {
			d.marks = map[int]addr.Address{}
		}
		bindSlotAddr(d.marks, r.Slot, addr.Address(r.A))
		return control.Reply{OK: true, Snapshot: d.snapshot()}
	case "stop":
		close(d.stop)
		return control.Reply{OK: true}
	default:
		return control.Reply{OK: false, Err: "unknown op: " + r.Op}
	}
}

func toAddrs(ss []string) []addr.Address {
	out := make([]addr.Address, 0, len(ss))
	for _, s := range ss {
		out = append(out, addr.Address(s))
	}
	return out
}

// bindSlotAddr binds a to a slot, ensuring one slot per bubble (mirrors the TUI).
func bindSlotAddr(marks map[int]addr.Address, slot int, a addr.Address) {
	for s, x := range marks {
		if x == a && s != slot {
			delete(marks, s)
		}
	}
	marks[slot] = a
}
