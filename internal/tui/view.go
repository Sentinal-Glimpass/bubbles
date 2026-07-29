package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	helpStyle  = lipgloss.NewStyle().Faint(true)
	pingStyle  = lipgloss.NewStyle().Bold(true)
	pingBlink  = lipgloss.NewStyle().Reverse(true).Bold(true)
)

var (
	panelStyle = lipgloss.NewStyle().Faint(true)
	panelHead  = lipgloss.NewStyle().Bold(true)
	flashStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
)

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// humanBytes renders a byte count compactly: 900K, 4.2M, 1.1G.
func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// hrGood is the green used for the headroom ON indicator and positive savings.
var hrGood = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))

// humanUSD renders a dollar amount compactly ($3.5K, $412, $7).
func humanUSD(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("$%.1fK", v/1000)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// humanCount renders a token count compactly (1.2M, 34K, 812).
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// sevStyle colors a usage figure by its limit severity.
func sevStyle(sev string) lipgloss.Style {
	switch sev {
	case "warning":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	case "high", "critical", "exceeded":
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")) // red
	case "good":
		// Reuses hrGood, the green already used for the headroom ON indicator, so
		// the panel keeps one green rather than gaining a second. Only the fleet
		// health block passes "good"; every existing caller's severity strings are
		// untouched and still land on the default.
		return hrGood
	default:
		return panelStyle
	}
}

// resetIn renders a compact "resets in" ("3h", "2d"); "" if unknown.
func resetIn(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := time.Until(at)
	switch {
	case d <= 0:
		return "soon"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// claudeUsageRows renders the account usage ("/usage") — one row per window
// (daily / weekly / model-scoped), each colored by the API's severity with its
// reset time.
func claudeUsageRows(c ClaudeUsage) []string {
	if !c.OK || len(c.Windows) == 0 {
		return nil
	}
	rows := []string{panelHead.Render("CLAUDE USAGE")}
	for _, w := range c.Windows {
		reset := ""
		if r := resetIn(w.ResetsAt); r != "" {
			reset = panelStyle.Render(" · " + r)
		}
		rows = append(rows, fmt.Sprintf(" %-13s ", w.Label)+sevStyle(w.Sev).Render(fmt.Sprintf("%3d%%", w.Pct))+reset)
	}
	return rows
}

// headroomRows renders the token-compression status: an ON/warming indicator and
// the cumulative token savings once the proxy reports any.
func headroomRows(h Headroom) []string {
	if !h.On {
		return nil
	}
	status := sevStyle("warning").Render("warming")
	if h.Ready {
		status = hrGood.Render("ON")
	}
	rows := []string{panelHead.Render("HEADROOM") + " " + status}
	if h.Ready {
		if h.SavingsPct <= 0 {
			rows = append(rows, panelStyle.Render(" saved   —  (no data yet)"))
		} else {
			saved := hrGood.Render(fmt.Sprintf("%.0f%%", h.SavingsPct))
			rows = append(rows, " saved "+saved+panelStyle.Render("  "+humanUSD(h.SavedUSD)))
		}
	}
	return rows
}

// healthRowWidth is the widest the FLEET alert row is allowed to get. The panel
// has no width cap of its own — overlayTopRight sizes it from the widest row
// emitted and clips the TREE to fit around it — so an unbounded alert row would
// silently eat the fleet tree. This is that bound.
// It is sized to hold all five alert segments at once ("stuck 1 · crash 2 ·
// ctx 3 · chk 4 · hung 5"), because a fleet in which all five are firing is
// exactly the one whose operator must not have any of them hidden.
const healthRowWidth = 44

// healthBudget resolves the alert row's column budget for a terminal of the
// given width. A width of 0 means "not known yet" (no WindowSizeMsg has
// arrived), which is not a reason to render nothing — the fixed cap applies.
// Otherwise the row never takes more than half the terminal, so the tree always
// keeps a readable share of a narrow one.
func healthBudget(width int) int {
	if width <= 0 {
		return healthRowWidth
	}
	if b := width / 2; b < healthRowWidth {
		return b
	}
	return healthRowWidth
}

// healthSegments builds the Phase 4 alert segments in the spec's priority order,
// which is also the drop order when the row doesn't fit, and the severity that
// colors them.
//
// THE CENTRAL RULE: a metric whose source is unavailable (nil) is OMITTED. It is
// never rendered as zero, because a zero on an operator panel reads as "verified
// healthy" and here it would mean "not measured" — the exact failure this panel
// exists to make impossible. A measured zero is also not rendered as a segment:
// it is not an alert, it is the absence of one, and it earns the green summary
// below instead of a column of noise.
func healthSegments(h FleetHealth) (segs []string, sev string) {
	add := func(n *int, label string) {
		if n != nil && *n > 0 {
			segs = append(segs, fmt.Sprintf("%s %d", label, *n))
		}
	}
	add(h.Stuck, "stuck")
	add(h.CrashLooping, "crash")
	add(h.OverContext, "ctx")
	add(h.FailingChecks, "chk")
	add(h.WedgedChecks, "hung")

	hot := func(n *int) bool { return n != nil && *n > 0 }
	switch {
	// Red: something is actually broken — a wedged bubble, a bubble the kernel
	// is no longer able to relaunch, or a sweep that is dead or hung.
	case hot(h.Stuck), hot(h.CrashLooping), hot(h.FailingChecks), hot(h.WedgedChecks):
		sev = "critical"
	// Amber: a cost warning, not a breakage. Context growth predicts the next
	// expensive rewarm; a backlog means a bubble is behind on its mail.
	case hot(h.OverContext), h.Backlog > 20:
		sev = "warning"
	// Green only on EVIDENCE. It requires both of the sources the spec names —
	// "all checks pass and nothing is stuck" — to have actually reported. If
	// either is nil there is nothing to be reassured by, so the row is dropped.
	case h.Stuck != nil && h.FailingChecks != nil:
		sev = "good"
	default:
		sev = "normal"
	}
	return segs, sev
}

// fleetHealthRows renders the cost/health summary: a header with the hot/total
// count, the Phase 4 alert row (stuck / crash loops / context / failing and
// wedged checks), a counters line (mute/cap/inline), and a backlog line when
// non-zero. Pure over the struct so it's testable without a terminal; width is
// the terminal width, used only to bound the alert row.
//
// Styling is entirely panelHead / panelStyle / sevStyle: this is a new block in
// an established panel, not a new visual language.
func fleetHealthRows(h FleetHealth, width int) []string {
	rows := []string{panelHead.Render(fmt.Sprintf("FLEET · %d/%d hot", h.Hot, h.Total))}

	segs, hsev := healthSegments(h)
	if len(segs) > 0 {
		// Drop from the TAIL — lowest priority first — until the row fits. The
		// highest-priority segment is never dropped: a row saying only "stuck 3"
		// on a 20-column terminal is still the most important thing on screen,
		// and dropping it to respect a budget would hide precisely the failure
		// the budget exists to keep visible.
		budget := healthBudget(width)
		for len(segs) > 1 && lipgloss.Width(strings.Join(segs, " · ")) > budget {
			segs = segs[:len(segs)-1]
		}
		rows = append(rows, sevStyle(hsev).Render(strings.Join(segs, " · ")))
	} else if hsev == "good" {
		rows = append(rows, sevStyle("good").Render("checks ok"))
	}

	// The counters line keeps its own severity, unchanged: it reports on INV-1,
	// and a stuck bubble elsewhere in the fleet says nothing about whether the
	// flood ceiling is firing.
	sev := "normal"
	switch {
	case h.Capped > 0:
		sev = "critical" // INV-1 flood ceiling is actively firing
	case h.Backlog > 20:
		sev = "warning"
	}
	rows = append(rows, sevStyle(sev).Render(fmt.Sprintf("mute %d · cap %d · inline %d", h.Suppressed, h.Capped, h.Inlined)))
	if h.Backlog > 0 {
		rows = append(rows, panelStyle.Render(fmt.Sprintf("backlog %d", h.Backlog)))
	}
	return rows
}

// usagePanel builds the right-hand block: Claude account usage rows on top, then
// the resources view (RAM/CPU + hot count, always shown), then the top bubbles by
// CPU when any are live.
func usagePanel(u Model) []string {
	lines := claudeUsageRows(u.claude)
	lines = append(lines, headroomRows(u.headroom)...)
	// Resources are ALWAYS shown (even at 0 hot — RAM 0B · CPU 0%), so the metrics
	// corner never disappears when the fleet is idle.
	lines = append(lines,
		panelHead.Render(fmt.Sprintf("RESOURCES · %d hot", u.usage.Hot)),
		panelStyle.Render(fmt.Sprintf("RAM %s · CPU %.0f%%", humanBytes(u.usage.TotalMem), u.usage.TotalCPU)),
	)
	lines = append(lines, fleetHealthRows(u.health, u.width)...)
	for _, r := range u.usage.Top {
		name := r.Name
		if len(name) > 12 {
			name = name[:12]
		}
		lines = append(lines, panelStyle.Render(fmt.Sprintf("%-12s %6s %4.0f%%", name, humanBytes(r.Mem), r.CPU)))
	}
	return lines
}

// overlayTopRight keeps the usage panel pinned to the TOP-RIGHT corner over the
// top lines of body. To stop the tree from ever running into the metrics on a
// narrow terminal, each overlaid body row is ANSI-safely CLIPPED to the columns
// left of the panel (so a long row ends in an ellipsis instead of colliding).
// Widths are measured with lipgloss so styling doesn't skew alignment.
func overlayTopRight(body string, panel []string, width int) string {
	if len(panel) == 0 || width <= 0 {
		return body
	}
	const minGap = 2 // columns between the tree and the panel
	panelW := 0
	for _, p := range panel {
		if w := lipgloss.Width(p); w > panelW {
			panelW = w
		}
	}
	lines := strings.Split(body, "\n")
	for len(lines) < len(panel) {
		lines = append(lines, "")
	}
	avail := width - panelW - minGap // columns the tree may use on an overlaid row
	for i, p := range panel {
		if avail < 1 { // terminal too narrow for both: show the panel row alone
			lines[i] = p
			continue
		}
		bl := lines[i]
		if lipgloss.Width(bl) > avail {
			bl = ansi.Truncate(bl, avail, "…") // ANSI-aware, so color codes aren't cut mid-sequence
		}
		lines[i] = bl + strings.Repeat(" ", width-panelW-lipgloss.Width(bl)) + p
	}
	return strings.Join(lines, "\n")
}

// modelChoiceLabel renders the model cycle with the current choice bracketed,
// e.g. "[sonnet] opus fable".
func modelChoiceLabel(cur string) string {
	parts := make([]string, len(spawnModels))
	for i, mdl := range spawnModels {
		if mdl == cur {
			parts[i] = "[" + mdl + "]"
		} else {
			parts[i] = mdl
		}
	}
	return strings.Join(parts, " ")
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

// cursorLabel describes the highlighted bubble: "addr (role)".
func cursorLabel(m Model) string {
	a := m.curAddr()
	if a == "" {
		return "—"
	}
	label := a.String()
	if b, ok := m.k.Reg.Get(a); ok && b.Label() != "" {
		label += " (" + b.Label() + ")"
	}
	return label
}

// parentLabel describes the spawn parent for the prompt: "root" or "addr (role)".
func (m Model) parentLabel() string {
	if m.pendingParent.IsRoot() {
		return "root"
	}
	label := m.pendingParent.String()
	if b, ok := m.k.Reg.Get(m.pendingParent); ok && b.Label() != "" {
		label += " (" + b.Label() + ")"
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
	b.WriteString(titleStyle.Render("BUBBLES — fleet") + helpStyle.Render("   permissions: "+mode+" (ctrl+p)") + "\n")
	if m.Flash != "" {
		b.WriteString(flashStyle.Render(" "+m.Flash+" ") + "\n")
	}
	b.WriteString("\n")

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

		// Bottom-section header (TASKS / DEACTIVATED): selectable, collapsible with
		// →/←/enter.
		if r.sectionHead != "" {
			toggle := "▾"
			if m.sectionCollapsed[r.section] {
				toggle = "▸"
			}
			b.WriteString("\n" + cursor + toggle + " " + panelHead.Render(r.sectionHead) + "\n")
			continue
		}

		// Task verifier row: shown only in the TASKS section, labelled by the
		// task it guards. It disappears when the kernel reaps it on completion.
		if r.section == "task" {
			a := r.addr
			status := "○"
			if m.k.IsHot(a) {
				status = "●"
			}
			name := ""
			if bub, ok := m.k.Reg.Get(a); ok {
				name = bub.Label()
			}
			// ⟢ <task id> (<assigner> → <worker>) — who assigned it and to whom.
			line := fmt.Sprintf("%s  %s %s %s  ⟢ %s (%s → %s)", cursor, status, a, name, r.task, r.taskFrom, r.taskTo)
			b.WriteString(line + "\n")
			continue
		}

		// Deactivated row: shown only in the DEACTIVATED section, faint, with a
		// hint that 'x' re-activates.
		if r.section == "off" {
			a := r.addr
			name := ""
			if bub, ok := m.k.Reg.Get(a); ok {
				name = bub.Label()
			}
			b.WriteString(helpStyle.Render(fmt.Sprintf("%s  ⊘ %s %s  (x to re-activate)", cursor, a, name)) + "\n")
			continue
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
		persona := ""
		disabled := false
		alwaysOn := false
		if bub, ok := m.k.Reg.Get(a); ok {
			persona = bub.Label()
			disabled = bub.Disabled
			alwaysOn = bub.AlwaysOn
		}
		status := "○" // cold: no live session (paged out / never launched)
		if m.k.IsHot(a) {
			status = "●" // hot: resident, running
		}
		if disabled {
			status = "⊘" // parked: hidden from contacts, can't launch
		}
		if alwaysOn && !disabled {
			status = "◉" // always-on receiver: pinned hot, never misses mail
		}
		mark := ""
		if m.introStage > 0 || m.groupStage == 1 || m.groupEdit {
			mark = " "
			if m.introSet[a] || m.groupSet[a] || (m.groupEdit && m.inEditGroup(a)) {
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
		if !a.IsRoot() && m.k.Caps.CanSpawn(a) {
			line += " ⚡" // has the spawn grant
		}
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
		if disabled {
			line = helpStyle.Render(line + " (disabled)")
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
	switch {
	case m.spawnStage == 1:
		b.WriteString("new bubble under " + m.parentLabel() + " — name: " + m.input + "▏\n")
	case m.spawnStage == 2:
		b.WriteString("bubble '" + m.pendingPersona + "' under " + m.parentLabel() + " — pick a folder (↑/↓, enter):\n")
		for i, c := range m.folderChoices {
			cur := "  "
			if i == m.folderCursor {
				cur = "> "
			}
			b.WriteString("  " + cur + c.label + "\n")
		}
	case m.spawnStage == 3:
		b.WriteString("bubble '" + m.pendingPersona + "' under " + m.parentLabel() + " — options:\n")
		b.WriteString("  model: " + modelChoiceLabel(m.spawnModel) + "   (←/→ to change)\n")
		b.WriteString("  grant spawn ability (depth 1): " + onOff(m.spawnGrant) + "   ('s' toggles)\n")
		b.WriteString(helpStyle.Render("  [enter] create · [esc] cancel") + "\n")
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
	case m.editing:
		caret := ""
		if m.editField == 0 {
			caret = "▏"
		}
		sel := func(i int) string {
			if i == m.editField {
				return "> "
			}
			return "  "
		}
		b.WriteString("edit " + m.editAddr.String() + " — ↑/↓ field · enter save · esc cancel:\n")
		b.WriteString(sel(0) + "name: " + m.editPersona + caret + "\n")
		b.WriteString(sel(1) + "model:   " + modelChoiceLabel(m.editModel) + "   (←/→)\n")
		b.WriteString(sel(2) + "spawn grant (depth 1): " + onOff(m.editGrant) + "   (←/→ toggle)\n")
		b.WriteString(helpStyle.Render("  saving a model/grant change bounces the session so it takes effect now (conversation resumes)") + "\n")
	case m.groupEdit:
		g, _ := m.k.Groups.Get(m.groupEditName)
		b.WriteString(fmt.Sprintf("edit group '%s' (%d members) — ↑/↓ move · enter add/remove a bubble (✓) · esc done\n",
			m.groupEditName, len(g.Members)))
	case m.delBubble != "":
		n := descendantCount(m.k.Reg, m.delBubble)
		label := m.delBubble.String()
		if bub, ok := m.k.Reg.Get(m.delBubble); ok && bub.Label() != "" {
			label += " (" + bub.Label() + ")"
		}
		sub := ""
		if n > 0 {
			sub = fmt.Sprintf(" and its %d descendant(s)", n)
		}
		b.WriteString("delete " + label + sub + "? [y]es  [n]o\n")
	case m.groupDel && m.groupDelAsk:
		g, _ := m.k.Groups.Get(m.groupDelName)
		b.WriteString(fmt.Sprintf("delete group '%s' — also delete its %d member bubble(s)? [y]es  [n]o (keep them)  [esc] cancel\n",
			m.groupDelName, len(g.Members)))
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
		b.WriteString(helpStyle.Render("↑/↓ move · →/← expand · enter dive · 0-9 jump · m+0-9 slot · n new · e edit · d delete · x disable · w always-on · i introduce · g group · G del-group · ctrl+p perms · q quit") + "\n")
	}
	return overlayTopRight(b.String(), usagePanel(m), m.width)
}
