package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseClaudeUsage(t *testing.T) {
	var u usageResponse
	body := `{"five_hour":{"utilization":17.0,"resets_at":"2026-07-05T22:49:59.912705+00:00"},
	          "seven_day":{"utilization":86.4,"resets_at":"2026-07-07T18:00:00.912730+00:00"},
	          "limits":[{"kind":"session","severity":"normal"},{"kind":"weekly_all","severity":"warning"}]}`
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	c := parseClaudeUsage(u)
	if !c.OK || c.FiveHourPct != 17 || c.WeeklyPct != 86 { // 86.4 rounds to 86
		t.Fatalf("pct: %+v", c)
	}
	if c.FiveHourSev != "normal" || c.WeeklySev != "warning" {
		t.Fatalf("sev: %+v", c)
	}
	if c.WeeklyResets.IsZero() || c.FiveHourResets.IsZero() {
		t.Fatalf("resets not parsed: %+v", c)
	}
	if !c.WeeklyResets.Equal(time.Date(2026, 7, 7, 18, 0, 0, 912730000, time.UTC)) {
		t.Fatalf("weekly reset = %v", c.WeeklyResets)
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
	if !c.OK {
		t.Fatal("expected OK usage")
	}
	t.Logf("LIVE usage: 5h=%d%% (%s)  wk=%d%% (%s)", c.FiveHourPct, c.FiveHourSev, c.WeeklyPct, c.WeeklySev)
}
