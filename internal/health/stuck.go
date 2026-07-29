// Package health holds pure fleet-health policy: functions over snapshots,
// with every clock reading supplied by the caller. Nothing here imports the
// kernel, touches a session, or performs I/O, so every decision it makes is
// reproducible in a test without sleeping.
package health

import (
	"sort"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Sample is one observation of a hot bubble, taken by the caller. It is plain
// data on purpose: the detector must not be able to reach back into a session.
type Sample struct {
	Addr addr.Address
	// LastActivity is when the session last produced OUTPUT. Note it falls back
	// to launch time when nothing has ever been printed (see
	// runner.ptySession.LastActivity), so it is "warm" by default and can only
	// ever be used as corroboration, never as the sole signal.
	LastActivity time.Time
	// RecentOutput is the session's whole capped scrollback ring, not a tail.
	// It is compared, never parsed.
	RecentOutput string
	// UnreadMail is how many notifiable messages are waiting for this bubble —
	// i.e. work it has been handed and has not consumed.
	UnreadMail int
	// Alive is whether the underlying process is still running.
	Alive bool
}

// Config tunes the detector.
type Config struct {
	// Threshold is how long a bubble must have produced no new output, while
	// mail is pending, before it is reported. A non-positive Threshold disables
	// detection entirely (Stuck returns nil), so an unconfigured caller reports
	// nothing rather than everything.
	Threshold time.Duration
}

// Stuck returns the addresses that look wedged: hot, holding work they have not
// consumed, and producing nothing new.
//
// It is a pure function of two consecutive sample sets and a caller-supplied
// clock. It REPORTS ONLY — it never kills, restarts, notifies or wakes
// anything, and callers must not either. A false positive that disturbs a
// working bubble costs a full prompt-cache rewarm, which is precisely the cost
// this whole programme exists to remove; a missed detection costs nothing but a
// later report. The detector is therefore deliberately biased towards silence.
//
// A bubble is reported only when ALL of the following hold:
//
//  1. it is Alive — a dead session is the relaunch path's business, not ours;
//  2. it has pending notifiable mail — a quiet bubble with an empty inbox is
//     merely idle, which is EvictIdle's business, not ours;
//  3. it appeared in the PREVIOUS sample set too — a single observation can
//     never establish "unchanged";
//  4. its RecentOutput is byte-identical to the previous sample's — any change
//     at all, even a spinner frame, means the process is still producing, so a
//     bubble whose output moved is never reported however long it has run;
//  5. its LastActivity is at least Threshold old.
//
// Conditions 4 and 5 are ANDed, not ORed. Either alone has a known false
// positive: output can be unchanged simply because two samples landed inside
// one quiet moment of real work, and LastActivity can be stale on a bubble that
// is thinking rather than wedged.
//
// Signals deliberately NOT used:
//
//   - InputReady() is a one-way latch (runner.ptySession sets it true once —
//     including on boot-deadline timeout and on process death — and never
//     clears it). A bubble wedged on a permission prompt still reports true.
//   - Terminal marker strings (the resume menu labels, "No conversation
//     found"). Those describe a resume failure, not a wedge, and matching them
//     would report on text that may be minutes stale in the ring buffer. The
//     detector never inspects the CONTENT of RecentOutput, only whether it
//     changed.
//
// The result is sorted by address so callers and tests see a stable order.
func Stuck(c Config, prev, cur []Sample, now time.Time) []addr.Address {
	if c.Threshold <= 0 {
		return nil
	}
	prevOut := make(map[addr.Address]string, len(prev))
	for _, p := range prev {
		prevOut[p.Addr] = p.RecentOutput
	}
	var out []addr.Address
	for _, s := range cur {
		if !s.Alive || s.UnreadMail <= 0 {
			continue
		}
		if s.LastActivity.IsZero() {
			continue // unknown activity: refuse to guess
		}
		before, seen := prevOut[s.Addr]
		if !seen || before != s.RecentOutput {
			continue
		}
		if now.Sub(s.LastActivity) < c.Threshold {
			continue
		}
		out = append(out, s.Addr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
