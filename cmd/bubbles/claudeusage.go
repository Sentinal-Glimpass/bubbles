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
		Percent  int    `json:"percent"`
		Severity string `json:"severity"`
		ResetsAt string `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

func parseUsageTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
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

// parseClaudeUsage maps the API response to the panel's per-window rows: the
// 5-hour "daily" window, the weekly window, and the per-model (e.g. Fable) weekly
// window — in that order, each with its percent, severity, and reset time.
func parseClaudeUsage(u usageResponse) tui.ClaudeUsage {
	var daily, weekly, scoped *tui.UsageWindow
	for _, l := range u.Limits {
		w := tui.UsageWindow{Pct: l.Percent, Sev: l.Severity, ResetsAt: parseUsageTime(l.ResetsAt)}
		switch l.Kind {
		case "session":
			w.Label = "daily usage" // Anthropic's short limit is a 5-hour rolling window
			daily = &w
		case "weekly_all":
			w.Label = "weekly usage"
			weekly = &w
		case "weekly_scoped":
			name := "scoped"
			if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
				name = l.Scope.Model.DisplayName
			}
			w.Label = name + " usage"
			scoped = &w
		}
	}
	// fall back to the top-level windows if limits[] is absent
	if daily == nil && u.FiveHour.ResetsAt != "" {
		daily = &tui.UsageWindow{Label: "daily usage", Pct: int(u.FiveHour.Utilization + 0.5), ResetsAt: parseUsageTime(u.FiveHour.ResetsAt)}
	}
	if weekly == nil && u.SevenDay.ResetsAt != "" {
		weekly = &tui.UsageWindow{Label: "weekly usage", Pct: int(u.SevenDay.Utilization + 0.5), ResetsAt: parseUsageTime(u.SevenDay.ResetsAt)}
	}
	c := tui.ClaudeUsage{OK: true}
	for _, w := range []*tui.UsageWindow{daily, weekly, scoped} {
		if w != nil {
			c.Windows = append(c.Windows, *w)
		}
	}
	return c
}

// runClaudeUsage keeps the usage panel live: it re-sends the value to the TUI
// every second (so the row is always painted, in step with the other metrics and
// so reset countdowns tick), but only hits the API every 10s. A failed fetch
// (expired token / offline) keeps the last-known value on screen.
func runClaudeUsage(curProg interface{ Load() *tea.Program }) {
	if _, err := claudeAccessToken(); err != nil {
		return // not logged in with a subscription token — no usage to show
	}
	var last tui.ClaudeUsage
	var lastFetch time.Time
	fetch := func() {
		if u, err := fetchClaudeUsage(); err == nil {
			last, lastFetch = u, time.Now()
		}
	}
	send := func() {
		if last.OK {
			if p := curProg.Load(); p != nil {
				p.Send(tui.ClaudeUsageMsg(last))
			}
		}
	}
	fetch()
	send()
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		if time.Since(lastFetch) >= 10*time.Second {
			fetch()
		}
		send()
	}
}
