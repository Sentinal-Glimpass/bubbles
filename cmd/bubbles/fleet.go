package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
)

// bubbleRec is a persisted bubble (one entry in the fleet manifest).
type bubbleRec struct {
	Addr      string   `json:"addr"`
	Persona   string   `json:"persona"`
	Dir       string   `json:"dir"`
	Parent    string   `json:"parent"`
	SessionID string   `json:"sessionId"`
	Contacts  []string `json:"contacts"`
}

// manifest is the on-disk fleet for one workspace.
type manifest struct {
	Bubbles []bubbleRec       `json:"bubbles"`
	Marks   map[string]string `json:"marks"` // slot -> address
}

func fleetPath(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "fleet.json")
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
		for _, c := range k.Contacts(b.Addr) {
			cs = append(cs, c.String())
		}
		recs = append(recs, bubbleRec{
			Addr: b.Addr.String(), Persona: b.Persona, Dir: b.Dir,
			Parent: b.Parent.String(), SessionID: b.SessionID, Contacts: cs,
		})
	}
	mk := map[string]string{}
	for slot, a := range marks {
		mk[strconv.Itoa(slot)] = a.String()
	}
	data, err := json.MarshalIndent(manifest{Bubbles: recs, Marks: mk}, "", "  ")
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
		return manifest{}, false
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
			Addr: addr.Address(r.Addr), Persona: r.Persona, Dir: r.Dir,
			Parent: addr.Address(r.Parent), Status: registry.Idle, SessionID: r.SessionID,
		})
	}
	for _, r := range m.Bubbles { // contacts
		for _, c := range r.Contacts {
			k.Caps.AddContact(addr.Address(r.Addr), addr.Address(c))
		}
	}
	for _, r := range m.Bubbles { // relaunch sessions (resume conversations)
		_ = k.Relaunch(addr.Address(r.Addr), r.Dir, r.Persona, r.SessionID)
	}
	for slot, a := range m.Marks {
		if n, err := strconv.Atoi(slot); err == nil {
			marks[n] = addr.Address(a)
		}
	}
	return marks
}
