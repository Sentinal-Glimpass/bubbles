package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	"github.com/Sentinal-Glimpass/bubbles/internal/sched"
	"github.com/Sentinal-Glimpass/bubbles/internal/tui"
)

// leaderByte (Ctrl-\) and leaderByteAlt (Ctrl-/) are the in-bubble leader
// prefixes — both work identically so you don't have to remember which:
//
//	<leader> <leader>  -> fleet
//	<leader> <digit>   -> jump to that slot if bound, else bind the current bubble
//
// Everything else (incl. Esc, arrows) goes straight to claude.
const (
	leaderByte    = 0x1c // Ctrl-\
	leaderByteAlt = 0x1f // Ctrl-/ (sends US, 0x1f) — an easier-to-reach alias
)

// maxBubbleName caps a spawned bubble's display name, so a charter accidentally
// crammed into the name field fails fast instead of corrupting the fleet view.
const maxBubbleName = 48

// isLeader reports whether b is one of the interchangeable leader keys.
func isLeader(b byte) bool { return b == leaderByte || b == leaderByteAlt }

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

	// Token-compression proxy (opt-in, off by default). Started here so it warms
	// up in parallel with the rest of boot; env is injected later (routeWhenReady)
	// once /health is ready, before any bubble launches. Fail-open.
	headroom := startHeadroom(baseDir)
	defer headroom.stop()

	lr := runner.NewLocal()
	lr.CitizenPrompt = citizenPrompt
	// Fleet default model. Unset => "sonnet" (subscription). Set BUBBLES_MODEL to a
	// specific model id, or to "auto"/"env" to pass NO --model so claude uses its
	// own default (ANTHROPIC_MODEL) — the path for AWS Bedrock / Vertex, where the
	// subscription aliases may not resolve. Per-bubble models still override.
	if v, ok := os.LookupEnv("BUBBLES_MODEL"); ok {
		switch v {
		case "auto", "env", "inherit", "default", "-":
			lr.DefaultModel = "" // don't force a model
		default:
			lr.DefaultModel = v
		}
	}
	allowAll := true // default: launch bubbles with --dangerously-skip-permissions
	lr.AllowAll = &allowAll
	lr.MemMaxMB = 0 // no per-session hard cap (it was killing legit busy sessions); each runs in its own cgroup scope for measurement, bounded only by the global budget below
	k := kernel.New(lr)
	// Harness wiring: every spawn gets a private brain folder (keyed by address —
	// shared workspaces still mean separate brains), and durable fleet decisions
	// go through the kernel-serialized ledger, not concurrent file writes.
	k.BrainBase = filepath.Join(baseDir, ".bubbles", "brains")
	k.DecisionsPath = filepath.Join(baseDir, ".bubbles", "memory", "decisions.md")
	lr.BrainDir = func(a addr.Address) string { return filepath.Join(k.BrainBase, a.String()) }
	k.MemBudget = 45 << 30       // 45 GB total: sessions are packed by ACTUAL RAM; the coldest page out when the sum exceeds this
	k.IdleTimeout = 30 * time.Minute  // page out sessions silent (no output) this long; they resume on next use
	k.TypingWindow = 10 * time.Second // hold a focused bubble's messages while you're typing; deliver once you pause this long
	inheritedMCP := resolveMCPServers(mcpAllowList()) // curated operator servers bubbles inherit (e.g. playwright)
	lr.MCPConfig = func(a addr.Address) string {
		return mcpConfigJSON(self, sock, a, k.Caps.CanSpawn(a), inheritedMCP)
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
	go func() { // deliver a held message to the focused bubble as soon as you stop typing
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for range t.C {
			k.FlushHeldIfIdle()
		}
	}()
	go func() { // periodic inbox drain: page in cold bubbles with pending mail so none go unanswered
		t := time.NewTicker(time.Duration(messagePollMinutes()) * time.Minute)
		defer t.Stop()
		for range t.C {
			k.DrainInboxes()
		}
	}()
	go func() { // fast recovery: re-nudge already-running bubbles whose notice never landed (cheap PTY write)
		t := time.NewTicker(45 * time.Second)
		defer t.Stop()
		for range t.C {
			k.RecoverUnread(true)
		}
	}()
	go func() { // persist the message store shortly after it changes, so mail survives a restart
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var lastVer int64 = -1
		for range t.C {
			if v := k.Store.Version(); v != lastVer {
				lastVer = v
				_ = saveInbox(baseDir, k)
			}
		}
	}()
	go func() { // fire durable wake schedules: the always-alive daemon wakes bubbles on their triggers
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for range t.C {
			k.FireDue()
		}
	}()
	go func() { // persist the task ledger shortly after it changes, so enforced routes survive a restart
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var lastVer int64 = -1
		for range t.C {
			if v := k.Tasks.Version(); v != lastVer {
				lastVer = v
				_ = saveTasks(baseDir, k)
			}
		}
	}()
	go func() { // persist schedules shortly after they change, so wakes survive a restart
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var lastVer int64 = -1
		for range t.C {
			if v := k.Sched.Version(); v != lastVer {
				lastVer = v
				_ = saveSchedules(baseDir, k)
			}
		}
	}()

	// Incoming webhooks: per-bubble secret URLs so scripts/crons/external
	// services can message (and wake) a bubble programmatically.
	k.WebhookBase = startWebhookServer(k)
	// One-command public exposure: --ngrok tunnels public -> the loopback webhook
	// port, so webhook() hands out internet-reachable URLs (daemon stays loopback).
	// An explicit --webhook-base wins over ngrok.
	if k.WebhookBase != "" && os.Getenv("BUBBLES_NGROK") == "1" && os.Getenv("BUBBLES_WEBHOOK_BASE") == "" {
		if url, _, nerr := startNgrok(webhookPort(), os.Getenv("BUBBLES_NGROK_DOMAIN")); nerr == nil {
			k.WebhookBase = url
			fmt.Fprintf(os.Stderr, "bubbles: webhooks public via ngrok at %s\n", url)
		} else {
			fmt.Fprintf(os.Stderr, "bubbles: ngrok tunnel failed: %v (webhooks stay loopback-only)\n", nerr)
		}
	}

	// Resource sampler: feeds the dashboard's top-right panel. It sends to
	// whichever TUI program is currently running (nil while diving into a bubble).
	var curProg atomic.Pointer[tea.Program]
	go runSampler(k, &curProg)
	go runClaudeUsage(&curProg)   // account /usage in the dashboard, polled directly (no claude session)
	go runHeadroomStats(&curProg) // token-compression savings in the dashboard (no-op unless --headroom)

	ln, err := ipc.Serve(sock, func(r ipc.Request) ipc.Reply { return handleIPC(k, r) })
	if err != nil {
		fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)

	// Gate compression routing on the proxy being healthy, BEFORE any bubble
	// launches (env is inherited by each claude session). Fail-open: if it isn't
	// ready in time, sessions talk to the provider directly.
	if headroom != nil {
		headroom.routeWhenReady(20 * time.Second)
	}

	// Quit/relaunch loop: the TUI quits when you dive in; we hand over the
	// terminal, then relaunch the fleet view.
	marks := restoreFleet(baseDir, k) // rehydrate a saved fleet (empty if none)
	inboxExisted, inboxOK := loadInbox(baseDir, k)
	loadSchedules(baseDir, k)
	loadTasks(baseDir, k)
	// After a restart, wake and re-notify every bubble that still has unread mail,
	// so a message that arrived before the stop lands in its terminal too. Delayed
	// a few seconds so the fleet and webhook/tunnel setup settle first.
	go func() { time.Sleep(6 * time.Second); k.RecoverUnread(false) }()
	startupFlash := ""
	if inboxExisted && !inboxOK {
		startupFlash = "⚠ saved inbox was unreadable — some messages may have been lost; ask senders to resend"
	}
	m := tui.New(k)
	m.BaseDir = baseDir
	m.Marks = marks
	m.AllowAll = &allowAll
	m.Flash = startupFlash
	// Persist promptly whenever the fleet changes (incl. agent-driven spawn/edit/
	// delete over IPC), so nothing is lost if the daemon dies before the next dive.
	m.OnPersist = func() { k.SyncSessionIDs(); _ = saveFleet(baseDir, k, marks); _ = saveInbox(baseDir, k) }
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
		_ = saveInbox(baseDir, k)        // persist mail
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
		_ = saveInbox(baseDir, k)        // persist mail
		m = prev.Refreshed()             // back to fleet: same expand state + cursor where we left
		m.Flash = flash
	}
}

// clampPct bounds a CPU percentage to [0,100] (guards against a measurement
// blip — a tiny dt or a usage_usec jump — momentarily overshooting).
func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
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
	ncpu := float64(runtime.NumCPU()) // normalize to % of the WHOLE machine, so 100% = all cores busy (not top's per-core %)
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
				if dt := now.Sub(p.at).Seconds(); dt > 0 && ncpu > 0 {
					pct = (s.CPU - p.cpu).Seconds() / dt * 100 / ncpu // core-seconds/wall -> % of all cores
				}
			}
			pct = clampPct(pct)
			prev[s.Addr] = prevSample{s.CPU, now}
			totalCPU += pct
			rows = append(rows, tui.UsageRow{Name: s.Name, Mem: s.Mem, CPU: pct})
		}
		totalCPU = clampPct(totalCPU)
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
			if c.IsRoot() || k.IsHot(c) {
				label += " ●awake" // running now: an urgent message wakes it instantly / it's the human
			} else {
				label += " ○asleep" // paged out: urgent will page it in
			}
			out[i] = label
		}
		return ipc.Reply{OK: true, Contacts: out}
	case "introduce":
		if err := k.IntroduceBy(from, addr.Address(r.Addr), addr.Address(r.To)); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true}
	case "broadcast":
		n := k.BroadcastBy(from, r.Subject, r.Body, r.Urgent)
		return ipc.Reply{OK: true, ID: n} // ID = how many bubbles were reached
	case "spawn":
		if len(r.Name) > maxBubbleName {
			return ipc.Reply{OK: false, Err: fmt.Sprintf("name too long (%d > %d chars) — keep 'name' short and put the task in 'description'", len(r.Name), maxBubbleName)}
		}
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
	case "compact":
		if err := k.Compact(from, r.Body); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true}
	case "schedule":
		trig, err := sched.ParseTrigger(r.Every, r.Daily)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		id, err := k.ScheduleBy(from, addr.Address(r.To), r.Subject, r.Body, trig, r.Urgent)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: id}
	case "unschedule":
		if err := k.UnscheduleBy(from, r.Addr); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true}
	case "schedules":
		return ipc.Reply{OK: true, Messages: k.SchedulesFor(from)}
	case "webhook":
		target := addr.Address(r.To)
		if r.To == "" {
			target = from
		}
		url, err := k.WebhookURLBy(from, target)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: url}
	case "webhook_rotate":
		target := addr.Address(r.To)
		if r.To == "" {
			target = from
		}
		url, err := k.RotateWebhookBy(from, target)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: url}
	case "control_webhook":
		var url string
		var err error
		if r.Rotate {
			url, err = k.RotateControlWebhook(from)
		} else {
			url, err = k.ControlWebhookURL(from)
		}
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: url}
	case "assign_task":
		var checklist []string
		for _, ln := range strings.Split(r.Checklist, "\n") {
			if ln = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "-")); ln != "" {
				checklist = append(checklist, ln)
			}
		}
		id, err := k.AssignTask(from, addr.Address(r.To), r.Body, r.Cmd, checklist)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: id}
	case "submit_task":
		out, err := k.SubmitTask(from, r.Task, r.Body)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: out}
	case "verdict":
		out, err := k.TaskVerdict(from, r.Task, r.Pass, r.Body)
		if err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true, Addr: out}
	case "tasks":
		return ipc.Reply{OK: true, Messages: k.TasksFor(from)}
	case "log_decision":
		if err := k.LogDecision(from, r.Body); err != nil {
			return ipc.Reply{OK: false, Err: err.Error()}
		}
		return ipc.Reply{OK: true}
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

// defaultMCPAllow is the curated set of the operator's own MCP servers that
// bubbles get by default — broadly useful for autonomous work, and replicable
// (stdio/command-based) so they actually launch headlessly. Any not present in
// the operator's config are simply skipped. Override with BUBBLES_MCP.
var defaultMCPAllow = []string{
	"playwright", "context7", "firecrawl", "web-scraper", "web-scraping",
	"react-bits-mcp", "github", "googlemaps", "mechmagnet",
}

// mcpAllowList returns which of the operator's MCP servers bubbles inherit:
// BUBBLES_MCP (comma-separated names, or "none") if set, else the curated
// default.
func mcpAllowList() []string {
	v := os.Getenv("BUBBLES_MCP")
	if v == "" {
		return defaultMCPAllow
	}
	if v == "none" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolveMCPServers pulls the named servers' definitions out of the operator's
// ~/.claude.json (global mcpServers first, then any project's), so bubbles can be
// given exactly those under --strict-mcp-config. Unknown names are skipped.
func resolveMCPServers(allow []string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if len(allow) == 0 {
		return out
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	want := map[string]bool{}
	for _, n := range allow {
		want[n] = true
	}

	// 1. ~/.mcp.json (the standard MCP config location)
	if data, err := os.ReadFile(filepath.Join(home, ".mcp.json")); err == nil {
		var mcp struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(data, &mcp) == nil {
			for name, def := range mcp.MCPServers {
				if want[name] {
					out[name] = def
				}
			}
		}
	}

	// 2. ~/.claude.json (global + per-project servers)
	if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			for name, def := range cfg.MCPServers {
				if want[name] && out[name] == nil {
					out[name] = def
				}
			}
			for _, p := range cfg.Projects {
				for name, def := range p.MCPServers {
					if want[name] && out[name] == nil {
						out[name] = def
					}
				}
			}
		}
	}

	return out
}

// mcpConfigJSON builds the inline --mcp-config JSON: our own mcp-stdio server
// (tagged with this bubble's address) plus the curated set of the operator's
// servers. With --strict-mcp-config this is EXACTLY what a bubble sees.
func mcpConfigJSON(exe, sock string, a addr.Address, spawnable bool, extra map[string]json.RawMessage) string {
	spawn := "0"
	if spawnable {
		spawn = "1"
	}
	servers := map[string]any{
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
	}
	for name, def := range extra {
		if name == "bubbles" { // never let an inherited server shadow ours
			continue
		}
		// Inject BUBBLE_ADDR into inherited servers so they can resolve
		// per-bubble identity (e.g. mechmagnet's key-file lookup).
		var parsed map[string]any
		if json.Unmarshal(def, &parsed) == nil {
			env, _ := parsed["env"].(map[string]any)
			if env == nil {
				env = map[string]any{}
			}
			env["BUBBLE_ADDR"] = a.String()
			parsed["env"] = env
			if patched, err := json.Marshal(parsed); err == nil {
				servers[name] = json.RawMessage(patched)
				continue
			}
		}
		servers[name] = def
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	return string(b)
}

// diveInto hands the terminal to a bubble's PTY. It returns "" to go back to the
// fleet, or the address of another bubble to switch directly into (Ctrl-Q num).
// diveStatus paints a persistent one-line footer identifying the bubble the
// operator is diving in. The bubble's PTY is sized one row short of the physical
// terminal and a scroll region protects that last row, so claude never draws
// over or scrolls away the footer. All writes to the terminal (claude's output
// and our repaints) go through here under one mutex, so they never interleave
// mid-escape-sequence.
type diveStatus struct {
	mu    sync.Mutex
	out   io.Writer
	label string
	cols  int
	rows  int // physical rows; the footer sits on row `rows`
	last  time.Time
}

func (d *diveStatus) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, err := d.out.Write(p)
	d.paintLocked(false)
	return n, err
}

// paintLocked redraws the footer (throttled unless forced). It re-asserts the
// scroll region each time so a claude `ESC[r` (region reset) can't leave the
// footer exposed to scrolling. Cursor is saved/restored around the paint so
// claude's own cursor position is undisturbed.
func (d *diveStatus) paintLocked(force bool) {
	if d.rows < 3 || d.cols < 1 {
		return
	}
	if !force && time.Since(d.last) < 120*time.Millisecond {
		return
	}
	d.last = time.Now()
	text := d.label
	if len([]rune(text)) > d.cols {
		text = string([]rune(text)[:d.cols])
	}
	fmt.Fprintf(d.out, "\x1b7\x1b[1;%dr\x1b[%d;1H\x1b[7m\x1b[2K%s\x1b[0m\x1b8", d.rows-1, d.rows, text)
}

func (d *diveStatus) resize(cols, rows int) {
	d.mu.Lock()
	d.cols, d.rows = cols, rows
	d.paintLocked(true)
	d.mu.Unlock()
}

// EnsureAlive heals a dead session first, so diving into a crashed bubble (or
// one whose resume id vanished) transparently relaunches it.
func diveInto(k *kernel.Kernel, a addr.Address, marks map[int]addr.Address) (next addr.Address, launched bool) {
	sess := k.EnsureAlive(a)
	ps, ok := sess.(runner.PTYSession)
	if !ok || ps == nil {
		return "", false // launch failed (bad dir/args, crashed on boot) — caller surfaces it
	}
	// Mark this bubble focused so incoming messages don't type into (and submit)
	// what the operator is writing; they're flushed when we leave.
	k.SetFocus(a)
	defer k.UnsetFocus(a)
	f := ps.PTY()

	// Footer identifying which bubble you're in, pinned to a reserved bottom row.
	label := " ● " + a.String()
	if b, ok := k.Reg.Get(a); ok && b.Label() != "" {
		label += "  " + b.Label()
	}
	label += "   —  Ctrl-\\ Ctrl-\\ back to fleet"

	// Reserve the footer only when the terminal is tall enough to spare a row.
	var status *diveStatus
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil && ws.Rows >= 3 {
		status = &diveStatus{out: os.Stdout, label: label, cols: int(ws.Cols), rows: int(ws.Rows)}
	}

	// sizeChild fills the bubble's PTY with the real terminal, minus the reserved
	// footer row when the footer is active, so claude never renders over it.
	sizeChild := func() {
		ws, err := pty.GetsizeFull(os.Stdin)
		if err != nil {
			return
		}
		child := *ws
		if status != nil && child.Rows > 1 {
			child.Rows--
		}
		_ = pty.Setsize(f, &child)
	}

	// Keep the child sized (and the footer repainted) across window resizes.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			sizeChild()
			if status != nil {
				if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
					status.resize(int(ws.Cols), int(ws.Rows))
				}
			}
		}
	}()
	defer signal.Stop(winch)

	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	fmt.Print("\x1b[?25h\x1b[2J\x1b[H") // show cursor (Bubbletea's alt-screen hides it) + clear for claude's redraw
	// On the way out, disable any mouse reporting / bracketed paste claude turned on.
	defer fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\r\n")
	if status != nil {
		defer fmt.Print("\x1b[r") // release the scroll region so the fleet view uses the full screen
		status.resize(status.cols, status.rows)
	}

	// Subscribe to the session's persistent output stream (replays recent
	// scrollback first, so we see claude's current frame, not a black screen).
	// We do NOT read f directly — the drainer owns the read side so claude is
	// always drained and never stalls, viewed or not. All terminal writes route
	// through `status` (when active) so claude's output and footer repaints share
	// one lock and never interleave.
	var out io.Writer = os.Stdout
	if status != nil {
		out = status
	}
	detach := ps.Attach(out)
	defer detach()

	// Force claude (an Ink TUI) to repaint — it only redraws on a size change, so
	// re-entering an idle bubble would otherwise show a stale frame. Nudge the
	// size, pause so Ink renders, then settle back to the correct child size.
	sizeChild()
	if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
		reserve := 0
		if status != nil {
			reserve = 1
		}
		jitter := *ws
		if int(jitter.Rows) > reserve+1 {
			jitter.Rows = uint16(int(jitter.Rows) - reserve - 1)
		}
		_ = pty.Setsize(f, &jitter)
		time.Sleep(60 * time.Millisecond)
		sizeChild()
	}
	if status != nil {
		status.resize(status.cols, status.rows)
	}

	// Input loop. Esc and everything else go straight to claude; the leader keys
	// (Ctrl-\ or Ctrl-/) are intercepted by the state machine.
	armed := false // true after a leader press
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return "", true
		}
		b := buf[0]
		k.NoteKeystroke() // mark active typing so messages hold until you pause
		if armed {
			armed = false
			switch {
			case isLeader(b): // leader leader -> fleet
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
		if isLeader(b) {
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
