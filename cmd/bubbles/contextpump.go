package main

import (
	"fmt"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/transcript"
)

// contextPumpThrottle is the minimum gap between two pump ACTIONS on the same
// bubble. It is the primary correctness property of this file, not a nicety.
//
// A bubble pays for its whole context on every turn, so context growth is a
// compounding cost and compaction is the only thing that bounds it. But the
// remedy is itself a turn: a nudge costs the bubble a model turn to read, and a
// forced /compact costs it a full summarization pass. Sweep runs every 2
// minutes, and a bubble sitting above the threshold stays above it until it
// acts -- which it cannot do instantly. Unthrottled, that is ~30 nudges an hour
// for a condition that has not changed, i.e. the pump would cost far more than
// the leak it exists to fix, and would look exactly like the 632fe95 flood one
// layer up.
//
// 30 minutes is chosen to be comfortably longer than a bubble's natural
// checkpoint interval: the nudge asks it to compact at its NEXT natural
// checkpoint, so re-asking before it has plausibly reached one is pure waste.
const contextPumpThrottle = 30 * time.Minute

// pumpContext is the tiered compaction pump. It measures every bubble's real
// context size and applies escalating pressure:
//
//	< ContextNudgeTokens  -> measure only
//	>= ContextNudgeTokens -> ask the bubble to compact at its next checkpoint
//	>= ContextForceTokens -> stop asking; type /compact
//
// MEASURE EVERYTHING, ACT ONLY WHERE ACTING IS FREE. Reading a transcript is
// read-only and therefore safe on hot and cold bubbles alike (unlike
// trimTranscripts, which may only rewrite files no process holds open), so the
// FContextTokens gauge is recorded for the whole fleet. Acting is another
// matter: both actions write to a live session, and neither is worth paging a
// cold bubble in for. A cold bubble is by definition not spending its context
// on anything, so its size is a fact to record, not a problem to solve; it will
// be nudged the next sweep after it is genuinely in use.
func (m *HealthManager) pumpContext() {
	if m.home == "" {
		return
	}
	now := time.Now()
	for _, b := range m.k.Reg.All() {
		// A bubble that has never run has no transcript, and root has no
		// conversation of its own. Neither is an error worth reporting.
		if b.Addr.IsRoot() || b.SessionID == "" || b.Dir == "" {
			continue
		}
		st, err := transcript.Read(convPath(m.home, b.Dir, b.SessionID))
		if err != nil {
			// Missing file, ErrNoUsage (only user turns so far), or an
			// unreadable line: all mean "no measurement this sweep", which is
			// normal on a young or idle bubble. Staying quiet here is
			// deliberate -- a per-sweep warning for a brand-new bubble would be
			// its own kind of flood, on the operator's terminal instead.
			continue
		}
		// Unconditional: FContextTokens is a gauge, not a counter, and the TUI
		// panel and later phases consume it whether or not the pump acted. Set,
		// never Add -- it is the present size, not a lifetime sum.
		m.k.Cost.Set(b.Addr, costmeter.FContextTokens, st.ContextTokens)

		if st.ContextTokens < transcript.ContextNudgeTokens {
			continue
		}
		// Checked before acting, claimed only after an action actually lands:
		// a nudge that was never written (cold bubble, ceiling denial) must not
		// buy 30 minutes of silence it never paid for.
		if !m.pumpDue(b.Addr, now) {
			continue
		}
		if st.ContextTokens >= transcript.ContextForceTokens {
			// Compact writes /compact to a LIVE session and errors on a cold
			// one; it is never allowed to wake anything.
			if err := m.k.Compact(b.Addr, ""); err == nil {
				m.markPumped(b.Addr, now)
			}
			continue
		}
		if m.k.SystemNotice(b.Addr, contextNudgeText(st.ContextTokens)) {
			m.markPumped(b.Addr, now)
		}
	}
}

// contextNudgeText is the nudge wording. It states the measured size (so the
// bubble can judge for itself rather than take the pump's word for it) and asks
// for compaction at a natural checkpoint rather than immediately -- compacting
// mid-task discards the working state the task needs, which is why this is a
// request at 500k and only becomes an order at 800k.
func contextNudgeText(tokens int64) string {
	return fmt.Sprintf("⚠ context is %s tokens (threshold %s) — please call compact() at your next natural checkpoint; it is billed in full on every turn.",
		humanTokens(tokens), humanTokens(transcript.ContextNudgeTokens))
}

// humanTokens renders a token count compactly ("550k", "1.2M").
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// pumpDue reports whether a's throttle window has elapsed. It does not claim
// the window -- see markPumped.
func (m *HealthManager) pumpDue(a addr.Address, now time.Time) bool {
	m.pumpMu.Lock()
	defer m.pumpMu.Unlock()
	last, ok := m.lastPump[a]
	return !ok || now.Sub(last) >= contextPumpThrottle
}

// markPumped records that an action actually reached a, starting its throttle
// window.
func (m *HealthManager) markPumped(a addr.Address, now time.Time) {
	m.pumpMu.Lock()
	m.lastPump[a] = now
	m.pumpMu.Unlock()
}
