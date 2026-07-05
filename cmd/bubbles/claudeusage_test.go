package main

import (
	"encoding/json"
	"testing"
)

func TestParseClaudeUsage(t *testing.T) {
	var u usageResponse
	body := `{"five_hour":{"utilization":8.0,"resets_at":"2026-07-06T03:49:59.7+00:00"},
	          "seven_day":{"utilization":88.0,"resets_at":"2026-07-07T18:00:00.9+00:00"},
	          "limits":[
	            {"kind":"session","percent":8,"severity":"normal","resets_at":"2026-07-06T03:49:59.7+00:00"},
	            {"kind":"weekly_all","percent":88,"severity":"warning","resets_at":"2026-07-07T18:00:00.9+00:00"},
	            {"kind":"weekly_scoped","percent":62,"severity":"normal","resets_at":"2026-07-07T18:00:00.9+00:00","scope":{"model":{"display_name":"Fable"}}}
	          ]}`
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	c := parseClaudeUsage(u)
	if !c.OK || len(c.Windows) != 3 {
		t.Fatalf("windows: %+v", c)
	}
	want := []struct {
		label string
		pct   int
		sev   string
	}{
		{"daily usage", 8, "normal"},
		{"weekly usage", 88, "warning"},
		{"Fable usage", 62, "normal"},
	}
	for i, w := range want {
		g := c.Windows[i]
		if g.Label != w.label || g.Pct != w.pct || g.Sev != w.sev {
			t.Fatalf("window %d = %+v want %+v", i, g, w)
		}
		if g.ResetsAt.IsZero() {
			t.Fatalf("window %d reset not parsed", i)
		}
	}
}

// TestFetchClaudeUsageLive hits the real endpoint if a token is present; it just
// confirms the wiring works end-to-end (skips if not logged in).
func TestFetchClaudeUsageLive(t *testing.T) {
	if _, err := claudeAccessToken(); err != nil {
		t.Skip("no claude token; skipping live usage fetch")
	}
	c, err := fetchClaudeUsage()
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if !c.OK || len(c.Windows) == 0 {
		t.Fatal("expected OK usage with windows")
	}
	for _, w := range c.Windows {
		t.Logf("LIVE %-13s %3d%% (%s)", w.Label, w.Pct, w.Sev)
	}
}
