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

// headroomStats is the subset of the proxy's /stats we render. We surface the
// COST savings (summary.cost.savings_pct), not raw token compression — on
// subscription mode the dominant win is cache alignment (prefix stabilization so
// the provider's prompt cache hits), which shows up as dollars saved, not fewer
// tokens. Raw token compression alone badly understates the real value.
type headroomStats struct {
	Summary struct {
		Cost struct {
			SavingsPct float64 `json:"savings_pct"`
			Breakdown  struct {
				CacheSavingsUSD       float64 `json:"cache_savings_usd"`
				CompressionSavingsUSD float64 `json:"compression_savings_usd"`
			} `json:"breakdown"`
		} `json:"cost"`
		Compression struct {
			TokensRemoved int64 `json:"total_tokens_removed"`
		} `json:"compression"`
	} `json:"summary"`
}

var headroomHTTP = &http.Client{Timeout: 2 * time.Second}

// fetchHeadroom reads the proxy's /stats and returns the cost savings %, dollars
// saved, and tokens removed. ready is false when the proxy isn't answering
// (fail-open on the dashboard too).
func fetchHeadroom(url string) (savingsPct, savedUSD float64, tokensRemoved int64, ready bool) {
	resp, err := headroomHTTP.Get(url + "/stats")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, 0, 0, false
	}
	defer resp.Body.Close()
	var s headroomStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, 0, 0, true // proxy is up but the shape changed; still "ready"
	}
	savingsPct = s.Summary.Cost.SavingsPct
	savedUSD = s.Summary.Cost.Breakdown.CacheSavingsUSD + s.Summary.Cost.Breakdown.CompressionSavingsUSD
	tokensRemoved = s.Summary.Compression.TokensRemoved
	return savingsPct, savedUSD, tokensRemoved, true
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
		pct, usd, tokens, ready := fetchHeadroom(url)
		if p := curProg.Load(); p != nil {
			p.Send(tui.HeadroomMsg(tui.Headroom{On: true, Ready: ready, SavingsPct: pct, SavedUSD: usd, TokensSaved: tokens}))
		}
	}
	send()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		send()
	}
}
