// Package tui is the Bubbletea zoomable fleet tree: a live list of bubbles with
// status, blink pings from workers, and dive-in selection.
package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	tea "github.com/charmbracelet/bubbletea"
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

	rows          []fleetRow
	cursor        int
	pings         map[addr.Address]string
	blinkOn       bool
	expanded      map[addr.Address]bool // which tree nodes show their children (root open by default)
	groupExpanded map[string]bool       // which group nodes show their members
	markSet       bool                  // armed by `m`: next digit (re)assigns the cursor bubble to that slot

	spawnStage     int          // 0 = none, 1 = entering persona, 2 = picking folder
	pendingParent  addr.Address // bubble the new one is created under
	pendingPersona string
	input          string
	folderChoices  []folderChoice
	folderCursor   int

	introStage int                   // 0 = none, 1 = selecting members
	introSet   map[addr.Address]bool // bubbles chosen for a group introduction

	groupStage   int                   // 0 = none, 1 = select members, 2 = name, 3 = options
	groupSet     map[addr.Address]bool // members chosen for a new group
	groupName    string
	groupIntro   bool // option: introduce all members on create
	groupSession bool // option: attach a coordinator session
	groupDel     bool // delete-a-group picker is open
	groupDelCur  int

	// Selected is set to the address the user dived into, then the program
	// quits so the caller (cmd/bubbles) can hand over the terminal.
	Selected addr.Address
	quitting bool
}

// fleetRow is one line in the fleet view: a tree bubble, a group header, or a
// member listed under an expanded group.
type fleetRow struct {
	addr   addr.Address // bubble address; "" only for a group header
	group  string       // group name (header rows, and member rows for context)
	header bool         // true => a group node (expandable, sibling of root)
	depth  int          // indent
}

// New builds a Model over a kernel. Root starts collapsed (minimized) and sits
// at the bottom, below the groups; expand it with → to reveal the fleet.
func New(k *kernel.Kernel) Model {
	m := Model{
		k:             k,
		pings:         map[addr.Address]string{},
		expanded:      map[addr.Address]bool{}, // root collapsed by default
		groupExpanded: map[string]bool{},
	}
	m.rows = m.fleetRows()
	return m
}

// fleetRows builds the full row list: each group as an expandable node at the
// top, then the location tree (root) at the bottom, minimized by default.
func (m Model) fleetRows() []fleetRow {
	sessions := map[addr.Address]bool{} // group coordinator sessions live under their group, not the tree
	for _, g := range m.k.Groups.All() {
		if g.Session != "" {
			sessions[g.Session] = true
		}
	}
	var out []fleetRow
	for _, g := range m.k.Groups.All() { // groups on top, outside the main root
		out = append(out, fleetRow{group: g.Name, header: true})
		if m.groupExpanded[g.Name] {
			for _, mem := range g.Members { // the coordinator session is reached via Enter on the group node
				out = append(out, fleetRow{addr: mem, group: g.Name, depth: 1})
			}
		}
	}
	for _, a := range buildRows(m.k.Reg, m.expanded) { // root subtree at the bottom
		if sessions[a] {
			continue
		}
		out = append(out, fleetRow{addr: a, depth: strings.Count(string(a), ".")})
	}
	return out
}

// bindSlot binds a to a number slot, ensuring it has at most one slot (clears it
// from any other slot first, so the displayed [N] can't flicker).
func bindSlot(marks map[int]addr.Address, slot int, a addr.Address) {
	for s, x := range marks {
		if x == a && s != slot {
			delete(marks, s)
		}
	}
	marks[slot] = a
}

func (m Model) curRow() fleetRow {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return fleetRow{}
}

func (m Model) curAddr() addr.Address { return m.curRow().addr }

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
	if m.groupStage > 0 {
		return m.updateGrouping(msg)
	}
	if m.groupDel {
		return m.updateGroupDelete(msg)
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
	case "up", "k": // cyclable: wraps to the bottom at the top
		if n := len(m.rows); n > 0 {
			m.cursor = (m.cursor - 1 + n) % n
		}
	case "down", "j": // cyclable: wraps to the top at the bottom
		if n := len(m.rows); n > 0 {
			m.cursor = (m.cursor + 1) % n
		}
	case "right", "l": // expand the highlighted node (tree bubble or group)
		r := m.curRow()
		if r.header {
			m.groupExpanded[r.group] = true
			m.rows = m.fleetRows()
		} else if a := r.addr; a != "" && !m.expanded[a] && len(m.k.Reg.Children(a)) > 0 {
			m.expanded[a] = true
			m.rows = m.fleetRows()
		}
	case "left", "h": // collapse the node, or hop to its parent
		r := m.curRow()
		if r.header {
			delete(m.groupExpanded, r.group)
			m.rows = m.fleetRows()
		} else if a := r.addr; a != "" {
			if m.expanded[a] && len(m.k.Reg.Children(a)) > 0 {
				delete(m.expanded, a)
				m.rows = m.fleetRows()
			} else if p, ok := a.Parent(); ok {
				for i, rr := range m.rows {
					if rr.addr == p && !rr.header {
						m.cursor = i
						break
					}
				}
			}
		}
		if m.cursor >= len(m.rows) {
			m.cursor = len(m.rows) - 1
		}
	case "enter":
		r := m.curRow()
		if r.header { // a group node: dive its session if any, else toggle it open
			if g, ok := m.k.Groups.Get(r.group); ok && g.Session != "" {
				m.Selected = g.Session
				return m, tea.Quit
			}
			m.groupExpanded[r.group] = !m.groupExpanded[r.group]
			m.rows = m.fleetRows()
		} else if a := r.addr; a != "" {
			if a.IsRoot() {
				_ = m.k.StartRoot(m.BaseDir) // launch root's own session, then dive in
			} else {
				delete(m.pings, a) // visiting clears the ping
			}
			m.Selected = a
			return m, tea.Quit
		}
	case "ctrl+p":
		if m.AllowAll != nil {
			*m.AllowAll = !*m.AllowAll // toggle permission mode for future spawns
		}
	case "n":
		a := m.curAddr()
		if a == "" {
			a = addr.Root // on a group header: spawn under root
		}
		m.spawnStage, m.input, m.pendingParent = 1, "", a
	case "m":
		m.markSet = true // arm: next digit (re)assigns the highlighted bubble
	case "i":
		if len(m.k.Reg.All()) > 2 { // need at least two non-root bubbles
			m.introStage = 1
			m.introSet = map[addr.Address]bool{}
		}
	case "g":
		if len(m.k.Reg.All()) > 1 { // at least one non-root bubble
			m.groupStage, m.groupSet, m.groupName = 1, map[addr.Address]bool{}, ""
			m.groupIntro, m.groupSession = false, false
		}
	case "G":
		if len(m.k.Groups.All()) > 0 {
			m.groupDel, m.groupDelCur = true, 0
		}
	}
	return m, nil
}

// updateGrouping runs the 3-step group creator: pick members (✓), name it, then
// toggle options and create.
func (m Model) updateGrouping(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.groupStage {
	case 1: // select members
		switch msg.String() {
		case "esc":
			return m.clearGroup(), nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "enter":
			sel := m.curAddr()
			if sel == "" || sel.IsRoot() {
				return m, nil
			}
			if m.groupSet[sel] { // second enter on a ✓ -> go to naming
				m.groupStage = 2
			} else {
				m.groupSet[sel] = true
			}
		}
	case 2: // name
		switch msg.String() {
		case "esc":
			return m.clearGroup(), nil
		case "enter":
			if strings.TrimSpace(m.groupName) != "" {
				m.groupStage = 3
			}
		case "backspace":
			if len(m.groupName) > 0 {
				m.groupName = m.groupName[:len(m.groupName)-1]
			}
		default:
			if len(msg.Runes) > 0 {
				m.groupName += string(msg.Runes)
			}
		}
	case 3: // options + create
		switch msg.String() {
		case "esc":
			return m.clearGroup(), nil
		case "i":
			m.groupIntro = !m.groupIntro
		case "s":
			m.groupSession = !m.groupSession
		case "enter":
			name := strings.TrimSpace(m.groupName)
			var members []addr.Address
			for a := range m.groupSet {
				members = append(members, a)
			}
			m.k.CreateGroup(name, members, m.groupIntro)
			if m.groupSession {
				dir := filepath.Join(m.BaseDir, ".bubbles", "groups", name) // its own (gitignored) folder
				_ = os.MkdirAll(dir, 0o755)
				_, _ = m.k.AttachGroupSession(name, dir, runner.SpawnOpts{Persona: "#" + name})
			}
			m.rows = m.fleetRows()
			return m.clearGroup(), nil
		}
	}
	return m, nil
}

func (m Model) updateGroupDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	gs := m.k.Groups.All()
	switch msg.String() {
	case "esc":
		m.groupDel = false
	case "up", "k":
		if m.groupDelCur > 0 {
			m.groupDelCur--
		}
	case "down", "j":
		if m.groupDelCur < len(gs)-1 {
			m.groupDelCur++
		}
	case "enter", "d", "x":
		if m.groupDelCur < len(gs) {
			m.k.DeleteGroup(gs[m.groupDelCur].Name)
			m.rows = m.fleetRows() // a session bubble may have been removed
		}
		m.groupDel = false
	}
	return m, nil
}

func (m Model) clearGroup() Model {
	m.groupStage, m.groupSet, m.groupName = 0, nil, ""
	m.groupIntro, m.groupSession = false, false
	return m
}

func (m Model) handleDigit(slot int) (tea.Model, tea.Cmd) {
	if m.markSet { // `m` then digit: (re)assign, overwriting any existing binding
		m.markSet = false
		if cur := m.curAddr(); cur != "" && !cur.IsRoot() {
			if m.Marks == nil {
				m.Marks = map[int]addr.Address{}
			}
			bindSlot(m.Marks, slot, cur)
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
	cur := m.curAddr()
	if cur != "" && !cur.IsRoot() {
		if m.Marks == nil {
			m.Marks = map[int]addr.Address{}
		}
		bindSlot(m.Marks, slot, cur) // bind the highlighted bubble (one slot per bubble)
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
		sel := m.curAddr()
		if sel == "" || sel.IsRoot() {
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
			m.rows = m.fleetRows()
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
