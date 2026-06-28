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
	k         *kernel.Kernel
	Workspace string // base dir for new bubbles' folders

	rows     []addr.Address
	cursor   int
	pings    map[addr.Address]string
	blinkOn  bool
	spawning bool
	input    string

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
		if m.spawning {
			return m.updateSpawning(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		m.spawning = true
		m.input = ""
	}
	return m, nil
}

func (m Model) updateSpawning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.spawning = false
		m.input = ""
	case "enter":
		persona := strings.TrimSpace(m.input)
		if persona != "" {
			dir := filepath.Join(m.Workspace, persona)
			_ = os.MkdirAll(dir, 0o755)
			_, _ = m.k.Spawn(addr.Root, persona, dir, runner.SpawnOpts{Persona: persona})
			m.rows = buildRows(m.k.Reg)
		}
		m.spawning = false
		m.input = ""
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
