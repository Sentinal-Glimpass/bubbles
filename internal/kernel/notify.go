package kernel

import (
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// muteRules resolves a bubble's declared mute predicates for notify.Policy.
// It compiles on each call, which is cheap because notify caches compiled
// patterns package-wide; building the set here (rather than caching a RuleSet
// per bubble) means a rule edit takes effect on the very next message, with no
// invalidation step that could go stale.
func (k *Kernel) muteRules(a addr.Address) *notify.RuleSet {
	rules := k.Reg.MuteRules(a)
	if len(rules) == 0 {
		return nil
	}
	rs := notify.NewRuleSet()
	for _, r := range rules {
		_ = rs.Add(r) // a rule that fails to compile was rejected at creation; skip it rather than muting everything
	}
	return rs
}

// announced reports the backlog size last announced to a. It is the kernel's
// half of INV-2: the policy engine deliberately keeps no announced counter of
// its own, so this (cleared by Inbox) is the single source of truth for "this
// backlog has already been advertised".
func (k *Kernel) announced(a addr.Address) int {
	k.notifyMu.Lock()
	defer k.notifyMu.Unlock()
	return k.notified[a]
}

// markAnnounced records that a backlog of n was just advertised to a. The
// timestamp feeds the stale-notice recovery sweep, so a notice that never
// landed is still eventually retried.
func (k *Kernel) markAnnounced(a addr.Address, n int) {
	k.notifyMu.Lock()
	k.notified[a] = n
	k.lastNudge[a] = time.Now()
	k.notifyMu.Unlock()
}

// announceOnce is the compare-and-set form of markAnnounced, for the operator
// paths (leaving a bubble, pausing typing) that flush a held backlog with a
// plain drain line rather than through the policy engine. It reports true at
// most once per backlog, so an overlapping flush and sweep don't stack two
// "you have mail" notices. Check and set are one critical section: splitting
// them is exactly the double-notification race.
func (k *Kernel) announceOnce(a addr.Address, n int) bool {
	if n == 0 {
		return false
	}
	k.notifyMu.Lock()
	defer k.notifyMu.Unlock()
	if k.notified[a] != 0 {
		return false
	}
	k.notified[a] = n
	k.lastNudge[a] = time.Now()
	return true
}

// decide runs the notification policy for a freshly-filed message, records the
// announcement, and records the cost of whatever it decides. It returns the
// decision, the announced level it replaced (so a delivery that turns out to
// be unreachable can put it back), and whether the caller should carry on to
// the delivery path at all.
//
// READ, DECIDE and MARK are ONE critical section, and that is the whole point
// of this function. Splitting them lets two concurrent deliveries to the same
// hot bubble both observe Announced == 0 and both write a notice -- the
// double-notification race that markNudge's compare-and-set used to close, and
// that -race will never report because it is a logic race, not a memory race.
// INV-1 bounds how bad it gets; it does not make it correct.
//
// Every suppression branch increments a counter: a suppression no counter
// records is indistinguishable from a message the kernel simply lost.
func (k *Kernel) decide(to addr.Address, id int, source, display, subject, body string, urgent, hot bool) (d notify.Decision, prev int, proceed bool) {
	k.notifyMu.Lock()
	prev = k.notified[to]
	st := notify.State{
		Notifiable: k.Store.NotifiableCount(to),
		Announced:  prev,
		Hot:        hot,
		AlwaysOn:   k.isAlwaysOn(to),
	}
	d = k.Notify.Decide(to, notify.Message{
		ID: id, Source: source, Display: display, Subject: subject, Body: body, Urgent: urgent,
	}, st, time.Now())
	if d.Action != notify.Suppress {
		// Claimed inside the same critical section as the read, so a
		// concurrent delivery sees the claim rather than a stale zero.
		k.notified[to] = d.Announce
		k.lastNudge[to] = time.Now()
	}
	k.notifyMu.Unlock()

	if d.MarkMuted {
		k.Store.SetMuted(id) // notification only: UnreadCount stays truthful and inbox() still shows it
		k.Cost.Add(to, costmeter.FNoticesSuppressed, 1)
	}
	if d.Action == notify.Suppress {
		switch {
		case d.MarkMuted: // already counted above
		case d.Capped:
			k.Cost.Add(to, costmeter.FNoticesCapped, 1)
		default:
			k.Cost.Add(to, costmeter.FNoticesSuppressed, 1)
		}
		return d, prev, false
	}
	return d, prev, true
}

// unclaimAnnounced puts the announced level back when the delivery the claim
// was made for turns out to be unreachable (a disabled bubble, a launch that
// failed). Without it such a message would be marked announced despite no
// notice existing, and would go unmentioned until the staleness sweep.
//
// It is a compare-and-set: if another delivery has claimed the level in the
// meantime, that claim is newer and must stand.
func (k *Kernel) unclaimAnnounced(a addr.Address, claimed, prev int) {
	k.notifyMu.Lock()
	if k.notified[a] == claimed {
		k.notified[a] = prev
	}
	k.notifyMu.Unlock()
}

// writeNotice types a decision's line into s, records what it cost, and
// reports whether the notice is on its way. The announced high-water is NOT
// set here -- decide claimed it atomically before the write, which is what
// makes concurrent deliveries safe.
//
// A session that has not rendered its input yet (still booting, or sitting on
// the resume menu) would swallow the line unsubmitted, so the write is handed
// to deliverWhenReady off the caller's path — the same discipline the send
// path has always used.
//
// Everything that treats the message as CONSUMED runs in onWritten, which
// fires only after a write has actually succeeded. A deferred write can time
// out or find a dead session, and marking an inlined body non-notifiable
// before that resolves would leave content that was never typed anywhere and
// never re-announced -- the same class of error as marking it read.
func (k *Kernel) writeNotice(to addr.Address, s runner.Session, d notify.Decision) bool {
	if s == nil || !s.Alive() || d.Text == "" {
		return false
	}
	onWritten := func() {
		k.Cost.Add(to, costmeter.FNoticesWritten, 1)
		// An inlined body has been delivered in full, so it must never earn
		// another notice -- otherwise the same content re-announces on every
		// later sweep. It is marked non-notifiable rather than READ: read
		// state belongs to the recipient's inbox() call, and flipping it here
		// would drop the message out of UnreadCount and out of inbox()
		// entirely, i.e. the kernel would have silently consumed mail on the
		// recipient's behalf. No message is ever dropped; only its
		// notification is spent.
		for _, id := range d.MarkRead {
			k.Store.SetMuted(id)
			k.Cost.Add(to, costmeter.FDeliveriesInline, 1)
		}
		if d.Action == notify.Notice {
			k.Cost.Add(to, costmeter.FDeliveriesViaTool, 1)
		}
	}

	line := []byte(d.Text)
	if s.InputReady() {
		if _, err := s.Write(line); err != nil {
			return false
		}
		onWritten()
		return true
	}
	go k.deliverWhenReadyThen(to, line, onWritten)
	return true
}

// DrainCoalesced writes any coalescing batches that have come due. Without it
// a batch of non-urgent followers would sit silent until the next message
// happened to arrive, which on a quiet fleet could be never.
//
// It deliberately never wakes a cold bubble: a batch of non-urgent mail is by
// definition not worth a prompt-cache rewarm, and the messages stay notifiable
// in the store, so the ordinary drain still picks them up.
func (k *Kernel) DrainCoalesced() {
	now := time.Now()
	for _, b := range k.Reg.All() {
		a := b.Addr
		if k.isFocused(a) && k.typingActive() {
			continue // don't submit the operator's half-typed line
		}
		// Liveness is checked BEFORE Pending, never after. Pending CONSUMES
		// the coalescing buffer as it returns it, so skipping afterwards on a
		// dead session would discard a due batch with no notice and no
		// counter -- a silent suppression, which nothing else in this system
		// is allowed to do either.
		s := k.session(a)
		if s == nil || !s.Alive() {
			continue
		}
		d, ok := k.Notify.Pending(a, now)
		if !ok {
			// Not-due and ceiling-denied both come back false; only the latter
			// is a suppression, and it must not be silent.
			if d.Capped {
				k.Cost.Add(a, costmeter.FNoticesCapped, 1)
			}
			continue
		}
		if k.writeNotice(a, s, d) {
			k.markAnnounced(a, d.Announce)
		}
	}
}
