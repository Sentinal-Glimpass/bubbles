// Package tui is the Bubbletea zoomable fleet tree: a live list of bubbles with
// status, blink pings from workers, and dive-in selection.
package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// PingMsg is delivered (via tea.Program.Send) when a worker sends root a message.
type PingMsg struct {
	From    addr.Address
	Subject string
}

type blinkTickMsg struct{}

// Model is the fleet tree.
type Model struct {
	k        *kernel.Kernel
	BaseDir  string               // dir where `bubbles` was launched; bubble folders are downstream of it
	Marks    map[int]addr.Address // shared number-slots: digit binds (if free) or jumps (if bound)
	AllowAll *bool                // shared permission toggle (Ctrl+P): true => --dangerously-skip-permissions

	rows     []addr.Address
	cursor   int
	pings    map[addr.Address]string
	blinkOn  bool
	expanded map[addr.Address]bool // which nodes show their children (root open by default)
	markSet  bool                  // armed by `m`: next digit (re)assigns the cursor bubble to that slot

	spawnStage     int // 0 = none, 1 = entering persona, 2 = picking folder
	pendingParent  addr.Address // bubble the new one is created under
	pendingPersona string
	input          string
	folderChoices  []folderChoice
	folderCursor   int

	introStage int                   // 0 = none, 1 = selecting members
	introSet   map[addr.Address]bool // bubbles chosen for a group introduction

	// Selected is set to the address the user dived into, then the program
	// quits so the caller (cmd/bubbles) can hand over the terminal.
	Selected addr.Address
	quitting bool
}

// New builds a Model over a kernel, rows seeded from the registry. Root starts
// expanded (top-level bubbles visible); deeper levels collapse until expanded.
func New(k *kernel.Kernel) Model {
	exp := map[addr.Address]bool{addr.Root: true}
	return Model{k: k, pings: map[addr.Address]string{}, expanded: exp, rows: buildRows(k.Reg, exp)}
}

func (m Model) Init() tea.Cmd { return blinkTick() }

func blinkTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return blinkTickMsg{} })
}

// buildRows returns addresses in depth-first tree order, children sorted. A
// node's children are included only if the node is expanded.
func buildRows(reg *registry.Registry, expanded map[addr.Address]bool) []addr.Address {
	var out []addr.Address
	var walk func(a addr.Address)
	walk = func(a addr.Address) {
		out = append(out, a)
		if !expanded[a] {
			return
		}
		ch := reg.Children(a)
		sort.Slice(ch, func(i, j int) bool { return ch[i].Addr < ch[j].Addr })
		for _, c := range ch {
			walk(c.Addr)
		}
	}
	walk(addr.Root)
	return out
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case blinkTickMsg:
		m.blinkOn = !m.blinkOn
		return m, blinkTick()
	case PingMsg:
		m.pings[msg.From] = msg.Subject
		return m, nil
	case tea.KeyMsg:
		if m.spawnStage > 0 {
			return m.updateSpawning(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.introStage > 0 {
		return m.updateIntroducing(msg)
	}
	// digits: jump to a bound slot / bind a free one, or (re)assign when armed with `m`
	if len(msg.Runes) == 1 && msg.Runes[0] >= '0' && msg.Runes[0] <= '9' {
		return m.handleDigit(int(msg.Runes[0] - '0'))
	}
	if msg.String() != "m" {
		m.markSet = false // any other key cancels a pending set
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "right", "l": // expand the highlighted node
		if len(m.rows) > 0 {
			a := m.rows[m.cursor]
			if !m.expanded[a] && len(m.k.Reg.Children(a)) > 0 {
				m.expanded[a] = true
				m.rows = buildRows(m.k.Reg, m.expanded)
			}
		}
	case "left", "h": // collapse the node, or hop to its parent
		if len(m.rows) > 0 {
			a := m.rows[m.cursor]
			if m.expanded[a] && len(m.k.Reg.Children(a)) > 0 {
				delete(m.expanded, a)
				m.rows = buildRows(m.k.Reg, m.expanded)
				if m.cursor >= len(m.rows) {
					m.cursor = len(m.rows) - 1
				}
			} else if p, ok := a.Parent(); ok {
				for i, r := range m.rows {
					if r == p {
						m.cursor = i
						break
					}
				}
			}
		}
	case "enter":
		if len(m.rows) > 0 {
			sel := m.rows[m.cursor]
			if !sel.IsRoot() { // can't dive into yourself
				delete(m.pings, sel) // visiting clears the ping
				m.Selected = sel
				return m, tea.Quit
			}
		}
	case "ctrl+p":
		if m.AllowAll != nil {
			*m.AllowAll = !*m.AllowAll // toggle permission mode for future spawns
		}
	case "n":
		m.spawnStage = 1
		m.input = ""
		m.pendingParent = addr.Root
		if len(m.rows) > 0 {
			m.pendingParent = m.rows[m.cursor] // spawn under the highlighted bubble
		}
	case "m":
		m.markSet = true // arm: next digit (re)assigns the highlighted bubble
	case "i":
		if len(m.rows) > 2 { // need at least two non-root bubbles
			m.introStage = 1
			m.introSet = map[addr.Address]bool{}
		}
	}
	return m, nil
}

func (m Model) handleDigit(slot int) (tea.Model, tea.Cmd) {
	if m.markSet { // `m` then digit: (re)assign, overwriting any existing binding
		m.markSet = false
		if cur := m.rows[m.cursor]; !cur.IsRoot() {
			if m.Marks == nil {
				m.Marks = map[int]addr.Address{}
			}
			m.Marks[slot] = cur
		}
		return m, nil
	}
	return m.handleMark(slot)
}

func (m Model) handleMark(slot int) (tea.Model, tea.Cmd) {
	if dest, ok := m.Marks[slot]; ok && dest != "" {
		if !dest.IsRoot() {
			m.Selected = dest // jump: dive into the bound bubble
			return m, tea.Quit
		}
		return m, nil
	}
	cur := m.rows[m.cursor]
	if !cur.IsRoot() {
		if m.Marks == nil {
			m.Marks = map[int]addr.Address{}
		}
		m.Marks[slot] = cur // bind the highlighted bubble
	}
	return m, nil
}

// updateIntroducing handles the group picker: enter adds the highlighted bubble
// to the set (✓); enter again on an already-selected bubble finalizes, making
// every member a mutual contact of every other. Root is skipped.
func (m Model) updateIntroducing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.introStage, m.introSet = 0, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "enter":
		sel := m.rows[m.cursor]
		if sel.IsRoot() {
			return m, nil // root already knows everyone
		}
		if m.introSet[sel] { // second enter on a ✓ bubble -> finalize
			m.finalizeIntroduce()
			m.introStage, m.introSet = 0, nil
		} else {
			m.introSet[sel] = true
		}
	}
	return m, nil
}

// finalizeIntroduce makes every selected bubble a mutual contact of every other.
func (m Model) finalizeIntroduce() {
	var members []addr.Address
	for a := range m.introSet {
		members = append(members, a)
	}
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			_ = m.k.Introduce(addr.Root, members[i], members[j])
		}
	}
}

// folderChoice is one option in the folder picker. folder is passed to
// resolveFolder (""=new ./persona subdir, "."=launch dir, "name"=base/name).
type folderChoice struct {
	label  string
	folder string
}

func (m Model) clearSpawn() Model {
	m.spawnStage, m.input, m.pendingPersona, m.pendingParent = 0, "", "", ""
	m.folderChoices, m.folderCursor = nil, 0
	return m
}

func (m Model) updateSpawning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		return m.clearSpawn(), nil
	}
	if m.spawnStage == 2 {
		return m.updateFolderPick(msg)
	}
	// stage 1: type the persona
	switch msg.String() {
	case "enter":
		persona := strings.TrimSpace(m.input)
		if persona == "" {
			return m.clearSpawn(), nil
		}
		m.pendingPersona, m.input = persona, ""
		m.folderChoices, m.folderCursor = listFolders(m.baseDirFor(m.pendingParent), persona), 0
		m.spawnStage = 2
	case "backspace":
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	default:
		if len(msg.Runes) > 0 {
			m.input += string(msg.Runes)
		}
	}
	return m, nil
}

func (m Model) updateFolderPick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.folderCursor > 0 {
			m.folderCursor--
		}
	case "down", "j":
		if m.folderCursor < len(m.folderChoices)-1 {
			m.folderCursor++
		}
	case "enter":
		if len(m.folderChoices) > 0 {
			c := m.folderChoices[m.folderCursor]
			dir := resolveFolder(m.baseDirFor(m.pendingParent), c.folder, m.pendingPersona)
			_ = os.MkdirAll(dir, 0o755)
			// root authorizes; the new bubble is attached under the selected parent
			_, _ = m.k.SpawnUnder(addr.Root, m.pendingParent, m.pendingPersona, dir, runner.SpawnOpts{Persona: m.pendingPersona})
			m.expanded[m.pendingParent] = true // reveal the new child
			m.rows = buildRows(m.k.Reg, m.expanded)
		}
		return m.clearSpawn(), nil
	}
	return m, nil
}

// baseDirFor returns the folder the picker is rooted at: a bubble's own folder
// (so children nest downstream of it), or the launch dir for root.
func (m Model) baseDirFor(parent addr.Address) string {
	if !parent.IsRoot() {
		if b, ok := m.k.Reg.Get(parent); ok && b.Dir != "" {
			return b.Dir
		}
	}
	return m.BaseDir
}

// listFolders builds the folder picker: "here", each immediate subdir of base
// (hidden ones skipped), then a "new ./persona" option.
func listFolders(base, persona string) []folderChoice {
	out := []folderChoice{{label: ". (here — whole project)", folder: "."}}
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, folderChoice{label: e.Name() + "/", folder: e.Name()})
			}
		}
	}
	out = append(out, folderChoice{label: "+ new folder: ./" + persona, folder: ""})
	return out
}

// resolveFolder maps a (possibly blank/relative) folder to an absolute path
// downstream of base. Blank defaults to base/<persona>.
func resolveFolder(base, folder, persona string) string {
	if folder == "" {
		folder = persona
	}
	if filepath.IsAbs(folder) {
		return folder
	}
	return filepath.Join(base, folder)
}
