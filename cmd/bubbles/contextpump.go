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
		// The tier is chosen BEFORE the throttle is consulted, and each tier
		// carries its own window. Gating the tier choice on a single shared
		// window would let the polite tier suppress the hard backstop: a bubble
		// nudged at 500k that then climbed past 800k would be unforceable for up
		// to 30 minutes, which is exactly the bubble the 800k tier was written
		// to catch (it is the one that did not act on the nudge). Escalation is
		// therefore always immediate on a tier transition, while each tier
		// remains rate-limited in its own right.
		if st.ContextTokens >= transcript.ContextForceTokens {
			if !m.pumpDue(b.Addr, pumpForce, now) {
				continue
			}
			// SystemCompact, NOT Compact: the automated path carries the same
			// input-safety guards as SystemNotice (operator typing-hold,
			// InputReady) and never wakes a cold bubble. Compact is the
			// interactive entry point and writes unconditionally, which on a
			// 2-minute ticker would submit /compact into the operator's
			// half-typed line, or hand it to a session still on the resume
			// menu where it is swallowed unsubmitted. It reports "written",
			// not "accepted", so the window below is only ever claimed for a
			// compaction that actually landed; a refusal is retried next sweep.
			if m.k.SystemCompact(b.Addr, "") {
				m.markPumped(b.Addr, pumpForce, now)
			}
			continue
		}
		if !m.pumpDue(b.Addr, pumpNudge, now) {
			continue
		}
		if m.k.SystemNotice(b.Addr, contextNudgeText(st.ContextTokens)) {
			m.markPumped(b.Addr, pumpNudge, now)
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

// pumpTier names the two escalation levels, so each can hold its own throttle
// window. They are separate because they answer different questions: the nudge
// window asks "have I already asked politely and given it time to act?", the
// force window asks "have I already compacted it for it?". One window for both
// would let the answer to the first suppress the second.
type pumpTier int

const (
	pumpNudge pumpTier = iota
	pumpForce
)

// pumpWindows is one bubble's per-tier throttle state.
type pumpWindows struct {
	nudge time.Time
	force time.Time
}

// pumpDue reports whether a's throttle window for tier has elapsed. It does not
// claim the window -- see markPumped.
func (m *HealthManager) pumpDue(a addr.Address, tier pumpTier, now time.Time) bool {
	m.pumpMu.Lock()
	defer m.pumpMu.Unlock()
	w := m.lastPump[a]
	last := w.nudge
	if tier == pumpForce {
		last = w.force
	}
	return last.IsZero() || now.Sub(last) >= contextPumpThrottle
}

// markPumped records that an action actually reached a, starting its throttle
// window.
//
// A force stamps BOTH windows; a nudge stamps only its own. Escalation is
// monotone: acting at the hard tier has already done everything the polite tier
// would have asked for, so re-asking politely in the minutes after a forced
// compaction (while the transcript still reflects the pre-compaction size) is
// pure waste. The converse must not hold, which is the whole point of Important
// 1 -- a polite ask must never satisfy the backstop.
//
// CAVEAT on "claimed only on success": SystemNotice returns true as soon as a
// not-yet-ready session's write is handed to deliverWhenReadyThen, and that
// deferred write can still find a dead session and return without writing. On
// that narrow branch the window is claimed for a notice that never landed. It
// is not worth a callback to close: the bubble is by then cold or dead, the
// FNoticesWritten counter records the truth either way, and the state self-heals
// on the next sweep after the window expires.
func (m *HealthManager) markPumped(a addr.Address, tier pumpTier, now time.Time) {
	m.pumpMu.Lock()
	w := m.lastPump[a]
	w.nudge = now
	if tier == pumpForce {
		w.force = now
	}
	m.lastPump[a] = w
	m.pumpMu.Unlock()
}
