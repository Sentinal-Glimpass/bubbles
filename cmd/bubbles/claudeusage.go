package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sentinal-Glimpass/bubbles/internal/tui"
)

// claudeUsageURL is the same account-usage endpoint the `/usage` slash command
// hits — account-level (identical for every bubble), so one poll covers the
// whole fleet with zero model tokens and no claude session.
const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// claudeAccessToken reads the OAuth bearer token from Claude Code's credential
// store. Re-read each poll: any running claude refreshes it before expiry, so a
// live fleet keeps it valid.
func claudeAccessToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", err
	}
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return "", err
	}
	if c.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no claude oauth token")
	}
	return c.ClaudeAiOauth.AccessToken, nil
}

// usageResponse is the subset of /api/oauth/usage we render.
type usageResponse struct {
	FiveHour struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
	Limits []struct {
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
	} `json:"limits"`
}

var usageHTTP = &http.Client{Timeout: 6 * time.Second}

// fetchClaudeUsage calls the usage endpoint and maps it to the dashboard shape.
func fetchClaudeUsage() (tui.ClaudeUsage, error) {
	tok, err := claudeAccessToken()
	if err != nil {
		return tui.ClaudeUsage{}, err
	}
	req, _ := http.NewRequest(http.MethodGet, claudeUsageURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := usageHTTP.Do(req)
	if err != nil {
		return tui.ClaudeUsage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tui.ClaudeUsage{}, fmt.Errorf("usage http %d", resp.StatusCode)
	}
	var u usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return tui.ClaudeUsage{}, err
	}
	return parseClaudeUsage(u), nil
}

// parseClaudeUsage maps the API response to the panel model (severity comes from
// the matching entry in limits[]; the weekly one is what usually bites).
func parseClaudeUsage(u usageResponse) tui.ClaudeUsage {
	sev := func(kinds ...string) string {
		for _, l := range u.Limits {
			for _, k := range kinds {
				if l.Kind == k {
					return l.Severity
				}
			}
		}
		return "normal"
	}
	reset := func(s string) time.Time {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil { // handles fractional seconds
			return t
		}
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	return tui.ClaudeUsage{
		OK:             true,
		FiveHourPct:    int(u.FiveHour.Utilization + 0.5),
		WeeklyPct:      int(u.SevenDay.Utilization + 0.5),
		FiveHourSev:    sev("session"),
		WeeklySev:      sev("weekly_all", "weekly"),
		FiveHourResets: reset(u.FiveHour.ResetsAt),
		WeeklyResets:   reset(u.SevenDay.ResetsAt),
	}
}

// runClaudeUsage polls the usage endpoint and pushes it to the current TUI.
// Usage moves slowly, so a once-a-minute GET is plenty. A failed fetch (expired
// token / offline) keeps the last-known value on screen rather than blanking it.
func runClaudeUsage(curProg interface{ Load() *tea.Program }) {
	if _, err := claudeAccessToken(); err != nil {
		return // not logged in with a subscription token — no usage to show
	}
	poll := func() {
		u, err := fetchClaudeUsage()
		if err != nil {
			return
		}
		if p := curProg.Load(); p != nil {
			p.Send(tui.ClaudeUsageMsg(u))
		}
	}
	poll() // first read promptly
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for range t.C {
		poll()
	}
}
