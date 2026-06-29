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
	k       *kernel.Kernel
	BaseDir string // dir where `bubbles` was launched; bubble folders are downstream of it

	rows    []addr.Address
	cursor  int
	pings   map[addr.Address]string
	blinkOn bool

	spawnStage     int // 0 = none, 1 = entering persona, 2 = entering folder
	pendingPersona string
	input          string

	introStage int                   // 0 = none, 1 = selecting members
	introSet   map[addr.Address]bool // bubbles chosen for a group introduction

	// Selected is set to the address the user dived into, then the program
	// quits so the caller (cmd/bubbles) can hand over the terminal.
	Selected addr.Address
	quitting bool
}

// New builds a Model over a kernel, rows seeded from the registry.
func New(k *kernel.Kernel) Model {
	return Model{k: k, pings: map[addr.Address]string{}, rows: buildRows(k.Reg)}
}

func (m Model) Init() tea.Cmd { return blinkTick() }

func blinkTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return blinkTickMsg{} })
}

// buildRows returns addresses in depth-first tree order, children sorted.
func buildRows(reg *registry.Registry) []addr.Address {
	var out []addr.Address
	var walk func(a addr.Address)
	walk = func(a addr.Address) {
		out = append(out, a)
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
	case "enter":
		if len(m.rows) > 0 {
			sel := m.rows[m.cursor]
			if !sel.IsRoot() { // can't dive into yourself
				delete(m.pings, sel) // visiting clears the ping
				m.Selected = sel
				return m, tea.Quit
			}
		}
	case "n":
		m.spawnStage = 1
		m.input = ""
	case "i":
		if len(m.rows) > 2 { // need at least two non-root bubbles
			m.introStage = 1
			m.introSet = map[addr.Address]bool{}
		}
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

func (m Model) updateSpawning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.spawnStage, m.input, m.pendingPersona = 0, "", ""
	case "enter":
		switch m.spawnStage {
		case 1: // persona entered → ask for folder
			persona := strings.TrimSpace(m.input)
			if persona == "" {
				m.spawnStage = 0
			} else {
				m.pendingPersona, m.input, m.spawnStage = persona, "", 2
			}
		case 2: // folder entered → spawn
			dir := resolveFolder(m.BaseDir, strings.TrimSpace(m.input), m.pendingPersona)
			_ = os.MkdirAll(dir, 0o755)
			_, _ = m.k.Spawn(addr.Root, m.pendingPersona, dir, runner.SpawnOpts{Persona: m.pendingPersona})
			m.rows = buildRows(m.k.Reg)
			m.spawnStage, m.input, m.pendingPersona = 0, "", ""
		}
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
