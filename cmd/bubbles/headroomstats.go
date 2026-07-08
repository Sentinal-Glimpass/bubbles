package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sentinal-Glimpass/bubbles/internal/tui"
)

// headroomStats is the subset of the proxy's /stats we render: cumulative tokens
// seen and saved, so the dashboard can show a live compression ratio.
type headroomStats struct {
	Summary struct {
		Compression struct {
			// The "with_cli_filtering" totals are cumulative and token-based,
			// covering both proxy compression and CLI output filtering.
			SavedTokens  int64 `json:"total_tokens_saved_with_cli_filtering"`
			BeforeTokens int64 `json:"total_tokens_before_with_cli_filtering"`
			TokensRemoved int64 `json:"total_tokens_removed"`
		} `json:"compression"`
	} `json:"summary"`
}

var headroomHTTP = &http.Client{Timeout: 2 * time.Second}

// fetchHeadroom reads the proxy's /stats and computes cumulative token savings.
// ready is false when the proxy isn't answering (fail-open on the dashboard too).
func fetchHeadroom(url string) (savingsPct float64, saved int64, ready bool) {
	resp, err := headroomHTTP.Get(url + "/stats")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, 0, false
	}
	defer resp.Body.Close()
	var s headroomStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, 0, true // proxy is up but the shape changed; still "ready"
	}
	c := s.Summary.Compression
	saved = c.SavedTokens
	if saved == 0 {
		saved = c.TokensRemoved
	}
	if c.BeforeTokens > 0 {
		savingsPct = float64(saved) / float64(c.BeforeTokens) * 100
	}
	return savingsPct, saved, true
}

// runHeadroomStats keeps the dashboard's HEADROOM row live when --headroom is on.
// It polls the local /stats endpoint every few seconds (cheap, loopback) and
// pushes the result to the TUI. No-op when compression is disabled.
func runHeadroomStats(curProg interface{ Load() *tea.Program }) {
	if os.Getenv("BUBBLES_HEADROOM") != "1" {
		return
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", headroomPort())
	send := func() {
		pct, saved, ready := fetchHeadroom(url)
		if p := curProg.Load(); p != nil {
			p.Send(tui.HeadroomMsg(tui.Headroom{On: true, Ready: ready, SavingsPct: pct, TokensSaved: saved}))
		}
	}
	send()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		send()
	}
}
