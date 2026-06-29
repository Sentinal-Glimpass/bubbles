package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	helpStyle  = lipgloss.NewStyle().Faint(true)
	pingStyle  = lipgloss.NewStyle().Bold(true)
	pingBlink  = lipgloss.NewStyle().Reverse(true).Bold(true)
)

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

	for i, a := range m.rows {
		depth := strings.Count(string(a), ".")
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		persona, status := "", "○"
		if bub, ok := m.k.Reg.Get(a); ok {
			persona, status = bub.Persona, dot(bub.Status)
		}
		mark := ""
		if m.introStage > 0 {
			mark = " "
			if m.introSet[a] {
				mark = "✓"
			}
		}
		line := fmt.Sprintf("%s%s%s%s %s %s", cursor, mark, strings.Repeat("  ", depth), status, a, persona)
		if slot, ok := slotOf[a]; ok {
			line += fmt.Sprintf(" [%d]", slot)
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
		b.WriteString("new bubble — persona: " + m.input + "▏\n")
	case m.spawnStage == 2:
		b.WriteString("bubble '" + m.pendingPersona + "' — pick a folder (↑/↓, enter):\n")
		for i, c := range m.folderChoices {
			cur := "  "
			if i == m.folderCursor {
				cur = "> "
			}
			b.WriteString("  " + cur + c.label + "\n")
		}
	case m.introStage == 1:
		b.WriteString("introduce — ↑/↓ + enter to add bubbles (✓); enter again on a ✓ bubble to finalize; esc cancels\n")
	default:
		b.WriteString(helpStyle.Render("↑/↓ move · enter dive · 0-9 bind/jump · n new · i introduce · q quit") + "\n")
	}
	return b.String()
}
