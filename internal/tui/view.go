package tui

import (
	"fmt"
	"strings"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	helpStyle  = lipgloss.NewStyle().Faint(true)
	pingStyle  = lipgloss.NewStyle().Bold(true)
	pingBlink  = lipgloss.NewStyle().Reverse(true).Bold(true)
)

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// descendantCount returns the total number of bubbles nested under a.
func descendantCount(reg *registry.Registry, a addr.Address) int {
	ch := reg.Children(a)
	n := len(ch)
	for _, c := range ch {
		n += descendantCount(reg, c.Addr)
	}
	return n
}

// descendantCountExcl is like descendantCount but skips hidden bubbles (group
// coordinator sessions), so a node's count matches what's shown in the tree.
func descendantCountExcl(reg *registry.Registry, a addr.Address, skip map[addr.Address]bool) int {
	n := 0
	for _, c := range reg.Children(a) {
		if skip[c.Addr] {
			continue
		}
		n += 1 + descendantCountExcl(reg, c.Addr, skip)
	}
	return n
}

func dot(s registry.Status) string {
	switch s {
	case registry.Working:
		return "●"
	case registry.Waiting:
		return "◐"
	case registry.Done:
		return "✓"
	default:
		return "○"
	}
}

// cursorLabel describes the highlighted bubble: "addr (role)".
func cursorLabel(m Model) string {
	a := m.curAddr()
	if a == "" {
		return "—"
	}
	label := a.String()
	if b, ok := m.k.Reg.Get(a); ok && b.Persona != "" {
		label += " (" + b.Persona + ")"
	}
	return label
}

// parentLabel describes the spawn parent for the prompt: "root" or "addr (role)".
func (m Model) parentLabel() string {
	if m.pendingParent.IsRoot() {
		return "root"
	}
	label := m.pendingParent.String()
	if b, ok := m.k.Reg.Get(m.pendingParent); ok && b.Persona != "" {
		label += " (" + b.Persona + ")"
	}
	return label
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	mode := "ask (acceptEdits)"
	if m.AllowAll != nil && *m.AllowAll {
		mode = "ALLOW-ALL (skip permissions)"
	}
	b.WriteString(titleStyle.Render("BUBBLES — fleet") + helpStyle.Render("   permissions: "+mode+" (ctrl+p)") + "\n\n")

	slotOf := map[addr.Address]int{}
	for slot, a := range m.Marks {
		slotOf[a] = slot
	}
	sessions := map[addr.Address]bool{} // hidden group sessions, excluded from tree counts
	for _, g := range m.k.Groups.All() {
		if g.Session != "" {
			sessions[g.Session] = true
		}
	}

	for i, r := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}

		// Group header row: an expandable node outside the main root.
		if r.header {
			g, _ := m.k.Groups.Get(r.group)
			toggle := "▸"
			if m.groupExpanded[r.group] {
				toggle = "▾"
			}
			sess := ""
			if g.Session != "" {
				sess = " ⟢ session"
			}
			b.WriteString(fmt.Sprintf("%s%s {%s} (%d)%s\n", cursor, toggle, r.group, len(g.Members), sess))
			continue
		}

		a := r.addr
		persona, status := "", "○"
		if bub, ok := m.k.Reg.Get(a); ok {
			persona, status = bub.Persona, dot(bub.Status)
		}
		mark := ""
		if m.introStage > 0 || m.groupStage == 1 {
			mark = " "
			if m.introSet[a] || m.groupSet[a] {
				mark = "✓"
			}
		}
		toggle, count := " ", ""
		if r.group == "" { // tree bubbles can expand their children; group members don't
			if nd := descendantCountExcl(m.k.Reg, a, sessions); nd > 0 {
				if m.expanded[a] {
					toggle = "▾"
				} else {
					toggle = "▸"
				}
				count = fmt.Sprintf(" (%d)", nd)
			}
		}
		line := fmt.Sprintf("%s%s%s%s %s %s %s%s", cursor, mark, strings.Repeat("  ", r.depth), toggle, status, a, persona, count)
		if slot, ok := slotOf[a]; ok {
			line += fmt.Sprintf(" [%d]", slot)
		}
		if r.group == "" { // show group tags only in the tree, not under a group node
			for _, gname := range m.k.Groups.Tags(a) {
				line += " {" + gname + "}"
			}
		}
		if !a.IsRoot() {
			if n := m.k.Store.UnreadCount(a); n > 0 {
				line += pingStyle.Render(fmt.Sprintf(" ✉%d", n))
			}
		}
		if subj, ok := m.pings[a]; ok {
			label := " ✉ " + subj + " "
			if m.blinkOn {
				label = pingBlink.Render(label)
			} else {
				label = pingStyle.Render(label)
			}
			line += "  " + label
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
	switch {
	case m.spawnStage == 1:
		b.WriteString("new bubble under " + m.parentLabel() + " — persona: " + m.input + "▏\n")
	case m.spawnStage == 2:
		b.WriteString("bubble '" + m.pendingPersona + "' under " + m.parentLabel() + " — pick a folder (↑/↓, enter):\n")
		for i, c := range m.folderChoices {
			cur := "  "
			if i == m.folderCursor {
				cur = "> "
			}
			b.WriteString("  " + cur + c.label + "\n")
		}
	case m.introStage == 1:
		b.WriteString("introduce — ↑/↓ + enter to add bubbles (✓); enter again on a ✓ bubble to finalize; esc cancels\n")
	case m.markSet:
		b.WriteString("set slot — press a digit (0-9) to assign " + cursorLabel(m) + " to it (esc cancels)\n")
	case m.groupStage == 1:
		b.WriteString("group — ↑/↓ + enter to add bubbles (✓); enter again on a ✓ to name it; esc cancels\n")
	case m.groupStage == 2:
		b.WriteString("group name: " + m.groupName + "▏ (enter to continue)\n")
	case m.groupStage == 3:
		b.WriteString(fmt.Sprintf("group '%s' — [i] introduce all: %s   [s] attach session: %s   [enter] create   [esc] cancel\n",
			m.groupName, onOff(m.groupIntro), onOff(m.groupSession)))
	case m.groupDel:
		b.WriteString("delete group — ↑/↓ select, enter to delete, esc cancel:\n")
		for i, g := range m.k.Groups.All() {
			cur := "  "
			if i == m.groupDelCur {
				cur = "> "
			}
			b.WriteString("  " + cur + g.Name + fmt.Sprintf(" (%d members)\n", len(g.Members)))
		}
	default:
		b.WriteString(helpStyle.Render("↑/↓ move · →/← expand/collapse · enter dive · 0-9 jump · n new · i introduce · g group · G del-group · q quit") + "\n")
	}
	return b.String()
}
