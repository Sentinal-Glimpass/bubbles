package kernel

import (
	"fmt"
	"sort"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
)

// This file holds the crash-loop backoff for the relaunch path. A bubble whose
// Dir no longer exists (or whose binary is broken) used to be relaunched
// forever: every attempt burned up to 3.3s of probe sleeps inside the calling
// goroutine and, worse, every attempt that DID come up re-paid a full boot
// context. Re-paying context for a bubble that cannot stay up is the exact
// waste this programme exists to remove, so consecutive failures now widen a
// suppression window and, past a threshold, stop the relaunch entirely.
//
// Modelled on headroomProc.supervise (cmd/bubbles/headroom.go): a consecutive
// failure counter, reset completely by one success, a terminal give-up after
// five, and fail-open — giving up means the fleet keeps running with that
// bubble cold, never that anything else stops. One deliberate difference: that
// supervisor's "backoff" is a retry budget with a flat 2s liveness probe,
// because a signal-0 probe is free. Here each retry costs a boot context, so
// the delay GROWS (doubling) and is CAPPED.
//
// WHERE THE STATE LIVES. Not in internal/sessions: that package is a leaf whose
// mutex must never span a launch or a Kill, and ensureAlive sleeps for seconds
// between deciding to launch and learning the outcome. It lives here behind its
// own small kernel mutex (the HealthManager.pumpMu / k.decisionsMu precedent),
// read BEFORE the launch and written AFTER it, with the lock released across
// runner.Launch, resumeHealthy and the relaunch probe.
//
// THE CHECK-THEN-ACT HAZARD. Reading the counter, dropping the lock, launching,
// then writing the result back is two critical sections around a slow
// operation, so two concurrent EnsureAlive calls for the SAME address can
// interleave. -race cannot see that: it is a logic race, not a memory race. It
// is closed with an epoch token rather than by holding the lock: relaunchAllowed
// hands back the epoch it observed, and a result is only applied if the epoch is
// still current. So a round of concurrent attempts records exactly ONE failure
// between them (not one each, which would give up in a fifth of the intended
// attempts), and a success that lands first cannot be undone by a slower
// failure from the same round.

const (
	// defaultRelaunchBackoff is the delay after the FIRST failed relaunch. It
	// doubles per consecutive failure. Chosen well above the ~3.3s an attempt
	// itself costs, so a crash-looping bubble spends its time cold rather than
	// re-attempting.
	defaultRelaunchBackoff = 10 * time.Second

	// defaultRelaunchBackoffCap ceilings the doubling, so a long-lived fleet
	// still re-tries a bubble whose Dir came back (a remounted volume, a fixed
	// config) within a bounded time instead of drifting towards never.
	defaultRelaunchBackoffCap = 5 * time.Minute

	// relaunchGiveUpAfter is the number of CONSECUTIVE failed relaunches after
	// which the bubble stops being relaunched at all. Same threshold, and same
	// meaning of "consecutive", as headroomProc.supervise.
	relaunchGiveUpAfter = 5
)

// RelaunchTrouble is one bubble's crash-loop state. It exists so that giving up
// on a bubble is REPORTED rather than silent: a suppression nothing can observe
// is indistinguishable from a bug, which this repo treats as a defect.
type RelaunchTrouble struct {
	Addr        addr.Address
	Fails       int       // consecutive failed relaunches
	GaveUp      bool      // true once Fails reached relaunchGiveUpAfter: no further relaunch is attempted
	NextAttempt time.Time // earliest time a relaunch will be tried again (zero once GaveUp)
}

// relaunchState is the per-address counter. epoch makes every read/write pair
// verifiable: see the check-then-act note above.
type relaunchState struct {
	fails  int
	epoch  uint64
	next   time.Time
	gaveUp bool
}

// relaunchBackoffBase and relaunchBackoffCap resolve the tunables, so a Kernel
// that was not built by New (or one a test zeroed) still backs off rather than
// silently crash-looping again.
func (k *Kernel) relaunchBackoffBase() time.Duration {
	if k.RelaunchBackoff > 0 {
		return k.RelaunchBackoff
	}
	return defaultRelaunchBackoff
}

func (k *Kernel) relaunchBackoffCap() time.Duration {
	if k.RelaunchBackoffCap > 0 {
		return k.RelaunchBackoffCap
	}
	return defaultRelaunchBackoffCap
}

// relaunchDelay is the suppression window after the nth consecutive failure:
// base doubled per failure, clamped to the cap. Pure, so the growth and the
// clamp are testable without launching anything.
func (k *Kernel) relaunchDelay(fails int) time.Duration {
	base, max := k.relaunchBackoffBase(), k.relaunchBackoffCap()
	if fails < 1 {
		return 0
	}
	if base >= max {
		return max
	}
	d := base
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= max || d <= 0 { // d <= 0 guards the overflow a huge base could reach
			return max
		}
	}
	return d
}

// relaunchAllowed reports whether a relaunch of a may be attempted now, and
// returns the epoch that the outcome must be reported against. epoch 0 means
// "no failure state recorded" — the healthy path, which costs one map lookup
// and touches nothing else.
func (k *Kernel) relaunchAllowed(a addr.Address) (uint64, bool) {
	k.backoffMu.Lock()
	defer k.backoffMu.Unlock()
	st := k.relaunch[a]
	if st == nil {
		return 0, true
	}
	if st.gaveUp {
		return st.epoch, false
	}
	if k.clockNow().Before(st.next) {
		return st.epoch, false
	}
	return st.epoch, true
}

// noteRelaunchFailure records that the attempt taken out under epoch failed. It
// returns the trouble to report when this failure is the one that gives up, so
// the announcement happens outside the lock.
func (k *Kernel) noteRelaunchFailure(a addr.Address, epoch uint64) (RelaunchTrouble, bool) {
	k.backoffMu.Lock()
	st := k.relaunch[a]
	switch {
	case st == nil && epoch != 0:
		// The state was cleared (a success, or an operator reset) while this
		// attempt was in flight: its verdict is stale, so it is dropped.
		k.backoffMu.Unlock()
		return RelaunchTrouble{}, false
	case st != nil && st.epoch != epoch:
		// Another attempt from the same round already recorded the outcome, or a
		// success intervened. Either way this one must not double-count.
		k.backoffMu.Unlock()
		return RelaunchTrouble{}, false
	case st == nil:
		if k.relaunch == nil {
			k.relaunch = map[addr.Address]*relaunchState{}
		}
		st = &relaunchState{}
		k.relaunch[a] = st
	}
	st.fails++
	st.epoch++
	report := false
	if st.fails >= relaunchGiveUpAfter {
		st.gaveUp = true
		st.next = time.Time{}
		report = true
	} else {
		st.next = k.clockNow().Add(k.relaunchDelay(st.fails))
	}
	out := RelaunchTrouble{Addr: a, Fails: st.fails, GaveUp: st.gaveUp, NextAttempt: st.next}
	k.backoffMu.Unlock()
	return out, report
}

// noteRelaunchFailed records a failed relaunch and, if that was the one that
// gave up, announces it. Split from noteRelaunchFailure so the announcement
// (which sends on the bus, and a handler may do anything) never runs under
// backoffMu.
func (k *Kernel) noteRelaunchFailed(a addr.Address, epoch uint64) {
	if t, report := k.noteRelaunchFailure(a, epoch); report {
		k.announceGiveUp(t)
	}
}

// noteRelaunchSuccess clears a's failure state COMPLETELY: counter, window and
// give-up flag. One good launch means the bubble is healthy again, so a bubble
// that failed four times and then came up is back to normal, exactly as a
// single good liveness probe resets headroomProc's counter.
func (k *Kernel) noteRelaunchSuccess(a addr.Address) {
	k.backoffMu.Lock()
	if _, ok := k.relaunch[a]; ok {
		delete(k.relaunch, a)
	}
	k.backoffMu.Unlock()
}

// announceGiveUp is the reporting half of the terminal decision. It pings root
// (the human dashboard's existing blink channel) so a bubble the fleet has
// stopped relaunching cannot fail silently, and leaves the state queryable via
// RelaunchTroubles for anything that wants to render it.
func (k *Kernel) announceGiveUp(t RelaunchTrouble) {
	name := t.Addr.String()
	if b, ok := k.Reg.Get(t.Addr); ok {
		name = b.Label()
	}
	_ = k.Bus.Send(bus.Message{
		From:    t.Addr,
		To:      addr.Root,
		Subject: "relaunch given up",
		Body: fmt.Sprintf("%s failed to launch %d times in a row and will not be relaunched. "+
			"Check its working directory, then re-enable it to try again.", name, t.Fails),
	})
}

// RelaunchTroubles lists every bubble currently in crash-loop backoff, worst
// first. This is the queryable side of "reported, not silent".
func (k *Kernel) RelaunchTroubles() []RelaunchTrouble {
	k.backoffMu.Lock()
	out := make([]RelaunchTrouble, 0, len(k.relaunch))
	for a, st := range k.relaunch {
		out = append(out, RelaunchTrouble{Addr: a, Fails: st.fails, GaveUp: st.gaveUp, NextAttempt: st.next})
	}
	k.backoffMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fails != out[j].Fails {
			return out[i].Fails > out[j].Fails
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// RelaunchTroubleFor returns a's crash-loop state, if it has one.
func (k *Kernel) RelaunchTroubleFor(a addr.Address) (RelaunchTrouble, bool) {
	k.backoffMu.Lock()
	defer k.backoffMu.Unlock()
	st := k.relaunch[a]
	if st == nil {
		return RelaunchTrouble{}, false
	}
	return RelaunchTrouble{Addr: a, Fails: st.fails, GaveUp: st.gaveUp, NextAttempt: st.next}, true
}

// ClearRelaunchFailures forgets a's crash-loop state, so a bubble the fleet gave
// up on can be retried once the operator has fixed whatever broke it. This is
// the deliberate un-park gesture; nothing time-based ever clears a give-up,
// because a bubble that failed five times in a row will fail the sixth too and
// re-paying that cost on a timer is the loop this change exists to break.
func (k *Kernel) ClearRelaunchFailures(a addr.Address) {
	k.noteRelaunchSuccess(a)
}
