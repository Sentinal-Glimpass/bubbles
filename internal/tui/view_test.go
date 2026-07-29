package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestOverlayKeepsPanelTopRight(t *testing.T) {
	body := "aardvark-a-very-long-tree-row-that-eats-the-width\nsecond"
	panel := []string{"USAGE 1234", "cpu 99%"}

	// Wide: panel sits to the RIGHT of the (untruncated) first body line.
	wide := overlayTopRight(body, panel, 120)
	first := strings.SplitN(wide, "\n", 2)[0]
	if !strings.Contains(first, "aardvark-a-very-long-tree-row") || !strings.Contains(first, "USAGE") {
		t.Fatalf("panel not overlaid top-right on a wide terminal: %q", first)
	}

	// Narrow: the panel STAYS top-right (never gone, never stacked); the tree row
	// is clipped with an ellipsis so it can't collide with the metrics.
	narrow := overlayTopRight(body, panel, 40)
	nfirst := strings.SplitN(narrow, "\n", 2)[0]
	if strings.HasPrefix(narrow, "USAGE") {
		t.Fatalf("panel should stay top-right, not stack on top: %q", narrow)
	}
	if !strings.Contains(nfirst, "USAGE 1234") {
		t.Fatalf("metrics missing from the narrow overlay: %q", nfirst)
	}
	if !strings.Contains(nfirst, "…") {
		t.Fatalf("long tree row should be clipped with an ellipsis: %q", nfirst)
	}
	if w := lipgloss.Width(nfirst); w > 40 {
		t.Fatalf("overlaid row wider than terminal (%d > 40): %q", w, nfirst)
	}
}

// TestFleetHealthRowsOmitUnavailableMetrics is the successor to the guard test
// that asserted "stuck" was never rendered at all. Phase 4 now MEASURES
// stuckness, so that premise is superseded — but the guarantee underneath it is
// not, and this asserts BOTH halves of it: an unmeasured source is still
// omitted, and a measured one is rendered. Only the second half would leave the
// panel free to print a reassuring 0 for something nobody looked at.
func TestFleetHealthRowsOmitUnavailableMetrics(t *testing.T) {
	base := FleetHealth{Hot: 2, Total: 5, Suppressed: 40, Capped: 0, Inlined: 12, Backlog: 3}

	// Source unavailable (nil): the original guarantee, preserved verbatim.
	joined := strings.Join(fleetHealthRows(base, 100), "\n")
	if !strings.Contains(joined, "40") {
		t.Fatal("suppressed count must be shown")
	}
	if strings.Contains(joined, "stuck") {
		t.Fatal("an unmeasured metric must be omitted, not rendered as zero")
	}

	// Measured zero: still not an alert, and still must not be mistaken for one.
	zeroed := base
	zeroed.Stuck = Measured(0)
	if strings.Contains(strings.Join(fleetHealthRows(zeroed, 100), "\n"), "stuck") {
		t.Fatal("a measured zero must not render as a stuck alert")
	}

	// Measured and non-zero: the row IS rendered.
	hot := base
	hot.Stuck = Measured(3)
	got := strings.Join(fleetHealthRows(hot, 100), "\n")
	if !strings.Contains(got, "stuck 3") {
		t.Fatalf("a measured stuck count must be rendered: %q", got)
	}
}

func TestFleetHealthRowsRenderEachMeasuredMetric(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*FleetHealth)
		want string
	}{
		{"stuck", func(h *FleetHealth) { h.Stuck = Measured(1) }, "stuck 1"},
		{"crash-loop", func(h *FleetHealth) { h.CrashLooping = Measured(2) }, "crash 2"},
		{"over-context", func(h *FleetHealth) { h.OverContext = Measured(3) }, "ctx 3"},
		{"failing-checks", func(h *FleetHealth) { h.FailingChecks = Measured(4) }, "chk 4"},
		{"wedged-checks", func(h *FleetHealth) { h.WedgedChecks = Measured(5) }, "hung 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h FleetHealth
			tc.set(&h)
			got := strings.Join(fleetHealthRows(h, 200), "\n")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("measured metric not rendered: want %q in %q", tc.want, got)
			}
			// And the same field left nil renders nothing at all.
			label := strings.Fields(tc.want)[0]
			if bare := strings.Join(fleetHealthRows(FleetHealth{}, 200), "\n"); strings.Contains(bare, label) {
				t.Fatalf("unmeasured %s must be omitted: %q", label, bare)
			}
		})
	}
}

func TestFleetHealthRowsSeverity(t *testing.T) {
	// sevStyle is the only source of colour; compare against what it renders so
	// the test asserts the CHOICE of severity, not an ANSI literal.
	red := sevStyle("critical").Render("x")
	amber := sevStyle("warning").Render("x")
	green := sevStyle("good").Render("x")
	prefix := func(s string) string { return strings.SplitN(s, "x", 2)[0] }

	// Green: both sources the spec names reported, and both are clean.
	ok := FleetHealth{Stuck: Measured(0), FailingChecks: Measured(0)}
	rows := fleetHealthRows(ok, 100)
	if !strings.HasPrefix(rows[1], prefix(green)) {
		t.Fatalf("all-clear must be green: %q", rows[1])
	}
	if !strings.Contains(rows[1], "checks ok") {
		t.Fatalf("all-clear row missing: %q", rows[1])
	}

	// Green requires EVIDENCE: with either source missing there is no row.
	half := FleetHealth{Stuck: Measured(0)}
	if got := strings.Join(fleetHealthRows(half, 100), "\n"); strings.Contains(got, "checks ok") {
		t.Fatalf("green must not be claimed without check evidence: %q", got)
	}

	// Amber: a cost warning, not a breakage.
	warn := fleetHealthRows(FleetHealth{Stuck: Measured(0), FailingChecks: Measured(0), OverContext: Measured(2)}, 100)
	if !strings.HasPrefix(warn[1], prefix(amber)) {
		t.Fatalf("context threshold must be amber: %q", warn[1])
	}

	// Red: each of the three breakages the spec names, plus a wedged check.
	for _, h := range []FleetHealth{
		{Stuck: Measured(1)},
		{CrashLooping: Measured(1)},
		{FailingChecks: Measured(1)},
		{WedgedChecks: Measured(1)},
	} {
		got := fleetHealthRows(h, 100)
		if !strings.HasPrefix(got[1], prefix(red)) {
			t.Fatalf("breakage must be red: %q", got[1])
		}
	}
}

func TestFleetHealthRowsDropLowestPriorityWhenNarrow(t *testing.T) {
	h := FleetHealth{
		Stuck: Measured(1), CrashLooping: Measured(2), OverContext: Measured(3),
		FailingChecks: Measured(4), WedgedChecks: Measured(5),
	}
	// Wide: everything fits, in priority order.
	wide := fleetHealthRows(h, 300)[1]
	for _, want := range []string{"stuck 1", "crash 2", "ctx 3", "chk 4", "hung 5"} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide row must carry every metric, missing %q: %q", want, wide)
		}
	}
	if i, j := strings.Index(wide, "stuck"), strings.Index(wide, "crash"); i > j {
		t.Fatalf("priority order violated: %q", wide)
	}

	// Narrower terminals drop from the TAIL, lowest priority first, and the row
	// never outgrows its budget.
	prevKept := 6
	for _, width := range []int{300, 90, 60, 45, 30, 20} {
		row := fleetHealthRows(h, width)[1]
		kept := strings.Count(row, "·") + 1
		if kept > prevKept {
			t.Fatalf("width %d kept MORE segments (%d > %d): %q", width, kept, prevKept, row)
		}
		prevKept = kept
		if kept < 5 && strings.Contains(row, "hung") {
			t.Fatalf("width %d dropped a segment but kept the lowest priority one: %q", width, row)
		}
		if !strings.Contains(row, "stuck 1") {
			t.Fatalf("width %d dropped the HIGHEST priority segment: %q", width, row)
		}
		if b := healthBudget(width); kept > 1 && lipgloss.Width(row) > b {
			t.Fatalf("width %d: row %q is %d cols, over budget %d", width, row, lipgloss.Width(row), b)
		}
	}
}

func TestUsagePanelIncludesFleetHealth(t *testing.T) {
	m := Model{}
	m.health = FleetHealth{Hot: 1, Total: 2, Suppressed: 7}
	joined := strings.Join(usagePanel(m), "\n")
	if !strings.Contains(joined, "FLEET") {
		t.Fatal("usagePanel must include the fleet health block")
	}
}
