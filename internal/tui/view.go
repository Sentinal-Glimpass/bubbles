package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
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
	b.WriteString(titleStyle.Render("BUBBLES — fleet") + "\n\n")

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
		line := fmt.Sprintf("%s%s%s %s %s", cursor, strings.Repeat("  ", depth), status, a, persona)
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
		b.WriteString("bubble '" + m.pendingPersona + "' — folder (blank = ./" + m.pendingPersona + "): " + m.input + "▏\n")
	case m.introStage == 1:
		b.WriteString("introduce — move to the FIRST bubble and press enter (esc cancels)\n")
	case m.introStage == 2:
		b.WriteString("introduce " + m.introFirst.String() + " ↔ pick the SECOND bubble, enter\n")
	default:
		b.WriteString(helpStyle.Render("↑/↓ move · enter dive · n new · i introduce · q quit") + "\n")
	}
	return b.String()
}
