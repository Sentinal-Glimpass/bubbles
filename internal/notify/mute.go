package notify

import (
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// muteWindow tracks one open mute window for a (bubble, rule) pair: when it
// opened and how many messages it has swallowed since, which is exactly the
// material the expiry rollup reports.
type muteWindow struct {
	opened  time.Time
	count   int
	source  string
	subject string
}

// mute is the first gate of Decide: it applies the recipient's mute rules and
// returns the decision they imply, or ok == false to let the message fall
// through to the remaining gates. It is a method on Policy and assumes p.mu is
// already held by Decide -- the whole decision, state mutations included, is
// one critical section, and splitting it is the double-notification race this
// engine exists to eliminate.
//
// It runs before any urgency handling: an urgent muted message must not page
// in a cold bubble, because that costs a full prompt-cache rewarm for traffic
// the bubble has already declared to be noise.
func (p *Policy) mute(to addr.Address, b *bubbleState, msg Message, st State, now time.Time) (Decision, bool) {
	rs := p.rules(to)
	if rs == nil {
		return Decision{}, false
	}
	// now is threaded in so an expired TTL stops the rule matching here, at
	// match time. notify never reads the clock itself.
	r, ok := rs.Match(msg.Source, msg.Subject, msg.Body, now)
	if !ok {
		return Decision{}, false
	}

	w := b.windows[r.ID]
	switch {
	case w == nil:
		// First match opens the window; this message still delivers, so the
		// bubble learns the traffic exists at least once.
		b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
		return Decision{}, false

	case now.Sub(w.opened) < r.Window:
		w.count++
		w.source, w.subject = msg.label(), msg.Subject
		return Decision{Action: Suppress, MarkMuted: true, Wake: false, Rule: r.ID}, true
	}

	// Window expired: report what was swallowed, then reopen.
	n, since, src, subj := w.count, w.opened, w.source, w.subject
	if n == 0 {
		// Nothing accumulated; reopen and deliver normally.
		b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
		return Decision{}, false
	}

	// Every path from here that does NOT produce a written rollup must leave
	// the window (and therefore n) intact. Resetting the count on a rollup the
	// caller will not write erases the only remaining record of the swallowed
	// messages -- they carry MarkMuted and so are not notifiable in the store,
	// meaning nothing else will ever mention them. That is the silent-stall
	// failure direction, and it has two shapes:
	//
	//  1. A rollup never wakes (see Wake: false below), so a COLD recipient
	//     cannot receive it: the kernel's delivery arm declines to write and
	//     the line is lost permanently. Suppress instead, and keep counting.
	//  2. The INV-1 ceiling denies the write, because a rollup is a real write.
	//
	// The cold check comes first so a rollup that could never be delivered does
	// not spend a ceiling token on the way to being dropped.
	if !st.Hot {
		w.count++
		w.source, w.subject = msg.label(), msg.Subject
		return Decision{Action: Suppress, MarkMuted: true, Wake: false, Rule: r.ID}, true
	}
	if !p.ceiling.Allow(to, now) {
		return Decision{Action: Suppress, Rule: r.ID, Capped: true}, true
	}

	// The message that triggers the rollup is deliberately neither delivered on
	// its own nor counted in the rollup it triggered: it is the first message
	// of the window it opens here, exactly like the first match that opened the
	// previous one.
	b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
	return Decision{
		Action: Rollup,
		// LOAD-BEARING WORDING: renderRollup's text ends with "call inbox() to
		// read." That standing instruction is the ONLY thing that covers the
		// trigger message, which is neither delivered nor marked muted here,
		// and which INV-2 (policy.go) then suppresses along with everything
		// after it because this decision sets Announce > 0. A copy edit that
		// drops the inbox() instruction from renderRollup silently reintroduces
		// a stall. Change the two together or not at all.
		Text: renderRollup(n, subj, src, since),
		Wake: false, // a rollup is by definition not worth a wake
		Rule: r.ID,
		// The message that triggered the rollup is not delivered and stays
		// notifiable, so the backlog it belongs to is what remains announced.
		Announce: st.Notifiable,
	}, true
}
