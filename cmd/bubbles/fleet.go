package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/inbox"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/sched"
	"github.com/Sentinal-Glimpass/bubbles/internal/tasks"
)

// bubbleRec is a persisted bubble (one entry in the fleet manifest).
type bubbleRec struct {
	Addr       string   `json:"addr"`
	Name       string   `json:"name,omitempty"`
	Persona    string   `json:"persona"`
	Dir        string   `json:"dir"`
	Parent     string   `json:"parent"`
	Model      string   `json:"model,omitempty"`
	Goal       string   `json:"goal,omitempty"`
	SpawnDepth int      `json:"spawnDepth,omitempty"` // spawn-grant depth (0 = none)
	SessionID  string   `json:"sessionId"`              // "" if never launched (lazy)
	Webhook    string   `json:"webhookToken,omitempty"` // incoming-webhook secret ("" = never minted)
	Control    string   `json:"controlToken,omitempty"` // control-webhook secret ("" = never minted)
	Disabled   bool     `json:"disabled,omitempty"`     // parked: hidden + can't launch until re-enabled
	Contacts   []string `json:"contacts"`
}

// groupRec is a persisted group.
type groupRec struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
	Session string   `json:"session,omitempty"`
}

// manifest is the on-disk fleet for one workspace.
type manifest struct {
	Bubbles []bubbleRec       `json:"bubbles"`
	Marks   map[string]string `json:"marks"` // slot -> address
	Groups  []groupRec        `json:"groups,omitempty"`
}

func fleetPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "fleet.json")
}

func inboxPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "inbox.json")
}

// inboxManifest is the on-disk message store: every message plus the ID sequence,
// so unread mail survives a restart and reply_to references still resolve.
type inboxManifest struct {
	Seq      int             `json:"seq"`
	Messages []inbox.Message `json:"messages"`
}

// tasksManifest is the on-disk task ledger: every task plus the ID sequence, so
// open tasks (and their enforced routes) survive a restart.
type tasksManifest struct {
	Seq   int          `json:"seq"`
	Tasks []tasks.Task `json:"tasks"`
}

func tasksPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "tasks.json")
}

// saveTasks persists the task ledger so enforced routes survive a restart.
func saveTasks(baseDir string, k *kernel.Kernel) error {
	ts, seq := k.Tasks.Snapshot()
	data, err := json.MarshalIndent(tasksManifest{Seq: seq, Tasks: ts}, "", "  ")
	if err != nil {
		return err
	}
	p := tasksPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// loadTasks restores the task ledger on startup (no-op if none saved).
func loadTasks(baseDir string, k *kernel.Kernel) {
	data, err := os.ReadFile(tasksPath(baseDir))
	if err != nil {
		return
	}
	var m tasksManifest
	if json.Unmarshal(data, &m) == nil {
		k.Tasks.Load(m.Tasks, m.Seq)
	}
}

func schedPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "schedules.json")
}

// saveSchedules persists durable wake schedules so they survive a restart.
func saveSchedules(baseDir string, k *kernel.Kernel) error {
	data, err := json.MarshalIndent(k.Sched.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	p := schedPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// loadSchedules restores wake schedules on startup (no-op if none saved).
func loadSchedules(baseDir string, k *kernel.Kernel) {
	data, err := os.ReadFile(schedPath(baseDir))
	if err != nil {
		return
	}
	var scs []sched.Schedule
	if json.Unmarshal(data, &scs) == nil {
		k.Sched.Load(scs)
	}
}

// saveInbox persists the message store next to the fleet manifest.
func saveInbox(baseDir string, k *kernel.Kernel) error {
	msgs, seq := k.Store.Snapshot()
	data, err := json.MarshalIndent(inboxManifest{Seq: seq, Messages: msgs}, "", "  ")
	if err != nil {
		return err
	}
	p := inboxPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// loadInbox restores the message store. Returns false if there was no saved
// inbox (fresh workspace); returns true with ok=false only handled by the caller
// when the file exists but is corrupt (so it can warn about lost mail).
func loadInbox(baseDir string, k *kernel.Kernel) (existed bool, ok bool) {
	data, err := os.ReadFile(inboxPath(baseDir))
	if err != nil {
		return false, true // no inbox yet: nothing lost
	}
	var m inboxManifest
	if json.Unmarshal(data, &m) != nil {
		return true, false // existed but corrupt: mail was lost
	}
	k.Store.Load(m.Messages, m.Seq)
	return true, true
}

// saveFleet writes the current fleet (bubbles, contacts, number-slots) to disk.
func saveFleet(baseDir string, k *kernel.Kernel, marks map[int]addr.Address) error {
	var recs []bubbleRec
	for _, b := range k.Reg.All() {
		if b.Addr.IsRoot() {
			if b.SessionID != "" { // root was started: persist it so it resumes
				recs = append(recs, bubbleRec{Addr: "0", Persona: "root", Dir: b.Dir, SessionID: b.SessionID})
			}
			continue
		}
		var cs []string
		for _, c := range k.Caps.Contacts(b.Addr) { // raw edges, not the display-filtered view — so edges to a disabled bubble survive
			cs = append(cs, c.String())
		}
		recs = append(recs, bubbleRec{
			Addr: b.Addr.String(), Name: b.Name, Persona: b.Persona, Dir: b.Dir,
			Parent: b.Parent.String(), Model: b.Model, Goal: b.Goal, SpawnDepth: k.Caps.SpawnDepth(b.Addr),
			SessionID: b.SessionID, Webhook: b.WebhookToken, Control: b.ControlToken, Disabled: b.Disabled, Contacts: cs,
		})
	}
	mk := map[string]string{}
	for slot, a := range marks {
		mk[strconv.Itoa(slot)] = a.String()
	}
	var grs []groupRec
	for _, g := range k.Groups.All() {
		ms := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			ms = append(ms, m.String())
		}
		grs = append(grs, groupRec{Name: g.Name, Members: ms, Session: g.Session.String()})
	}
	data, err := json.MarshalIndent(manifest{Bubbles: recs, Marks: mk, Groups: grs}, "", "  ")
	if err != nil {
		return err
	}
	p := fleetPath(baseDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// keep the .bubbles metadata dir out of the user's git
	_ = os.WriteFile(filepath.Join(filepath.Dir(p), ".gitignore"), []byte("*\n"), 0o644)
	return os.WriteFile(p, data, 0o644)
}

func loadFleet(baseDir string) (manifest, bool) {
	data, err := os.ReadFile(fleetPath(baseDir))
	if err != nil {
		return manifest{}, false
	}
	var m manifest
	if json.Unmarshal(data, &m) != nil {
		// Retry once — the daemon may be mid-write (atomic rename not guaranteed
		// on all filesystems).
		time.Sleep(100 * time.Millisecond)
		data, err = os.ReadFile(fleetPath(baseDir))
		if err != nil || json.Unmarshal(data, &m) != nil {
			return manifest{}, false
		}
	}
	return m, true
}

// restoreFleet rehydrates registry, contacts and number-slots from disk, then
// relaunches each bubble's claude (resuming its conversation). Returns the
// number-slot map (empty if there was no saved fleet).
func restoreFleet(baseDir string, k *kernel.Kernel) map[int]addr.Address {
	marks := map[int]addr.Address{}
	m, ok := loadFleet(baseDir)
	if !ok {
		return marks
	}
	for _, r := range m.Bubbles { // registry first, so addresses exist
		if addr.Address(r.Addr).IsRoot() { // root is pre-seeded; just restore its session info
			if b, ok := k.Reg.Get(addr.Root); ok {
				b.Dir, b.SessionID = r.Dir, r.SessionID
			}
			continue
		}
		k.Reg.Restore(registry.Bubble{
			Addr: addr.Address(r.Addr), Name: r.Name, Persona: r.Persona, Dir: r.Dir,
			Parent: addr.Address(r.Parent), Status: registry.Idle, Model: r.Model, Goal: r.Goal, SessionID: r.SessionID,
			WebhookToken: r.Webhook, ControlToken: r.Control, Disabled: r.Disabled,
		})
		if r.SpawnDepth > 0 {
			k.Caps.GrantSpawnDepth(addr.Address(r.Addr), r.SpawnDepth) // restore the spawn grant
		}
	}
	for _, r := range m.Bubbles { // contacts
		for _, c := range r.Contacts {
			k.Caps.AddContact(addr.Address(r.Addr), addr.Address(c))
		}
	}
	for _, r := range m.Bubbles { // re-apply parent->child contact (covers fleets saved before this rule)
		a := addr.Address(r.Addr)
		if p := addr.Address(r.Parent); p != "" && !a.IsRoot() {
			k.Caps.AddContact(p, a)
		}
	}
	// Lazy: restored bubbles stay COLD (0 RAM). Each keeps its saved SessionID, so
	// the first message/dive resumes its conversation (history intact); a bubble
	// that never launched has an empty SessionID and starts fresh on first use.
	// load number-slots, deduped: at most one slot per bubble (lowest slot wins),
	// so a stale multi-binding from an older save can't flicker.
	var slots []int
	for slot := range m.Marks {
		if n, err := strconv.Atoi(slot); err == nil {
			slots = append(slots, n)
		}
	}
	sort.Ints(slots)
	seen := map[addr.Address]bool{}
	for _, n := range slots {
		a := addr.Address(m.Marks[strconv.Itoa(n)])
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		marks[n] = a
	}
	for _, gr := range m.Groups { // groups (session bubble itself restored via Bubbles)
		var ms []addr.Address
		for _, s := range gr.Members {
			ms = append(ms, addr.Address(s))
		}
		k.Groups.Create(gr.Name, ms)
		if gr.Session != "" {
			k.Groups.SetSession(gr.Name, addr.Address(gr.Session))
		}
	}
	return marks
}
