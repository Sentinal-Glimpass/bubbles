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

// flushHeldBacklog is the operator paths' delivery step: a message held back
// while the operator was typing in the focused bubble is written as soon as
// they pause (FlushHeldIfIdle) or leave (UnsetFocus). That purpose is
// unchanged; what changed is that it now goes through the SAME decision point
// as the recovery sweep instead of reading UnreadCount and writing
// notify.RenderDrain straight to the PTY.
//
// Going around the policy engine here was the Task 8 defect surviving one
// layer up. UnreadCount counts muted messages (correctly -- they are unread,
// and inbox() still shows them), so a bubble whose entire backlog was noise it
// had explicitly muted got told "you have 200 unread" the moment the operator
// stopped typing: a full model turn spent on traffic the phase exists to make
// free, and the one bubble the operator is actually watching. It also bypassed
// INV-1 and recorded no FNoticesWritten, so the cost was invisible too.
//
// It never wakes a cold bubble: k.session returns nil rather than EnsureAlive,
// exactly as before, since a held backlog is not a reason to pay a rewarm.
func (k *Kernel) flushHeldBacklog(a addr.Address) {
	if a == "" {
		return
	}
	d, prev, ok := k.decideRecovery(a)
	if !ok {
		return
	}
	// No PTY write happens under notifyMu; writeNotice meters what it costs.
	if !k.writeNotice(a, k.session(a), d) {
		k.unclaimAnnounced(a, d.Announce, prev)
	}
}

// SystemNotice types a kernel-originated instruction into a's live session --
// a direct terminal line addressed to the bubble itself, NOT inbox mail. It
// reports whether the notice is on its way.
//
// Filing this as a message would be the wrong shape twice over: the recipient
// would have to spend an inbox() tool call to read a one-line instruction, and
// the instruction would be queued behind (and deduped against) its actual mail
// by INV-2. The content is not correspondence; it is the kernel telling a
// bubble something about itself.
//
// It is a method rather than a raw s.Write because every constraint that makes
// notifications affordable lives on this path and nowhere else:
//
//   - INV-1, so a caller on a ticker can never flood a bubble no matter how
//     badly it throttles itself;
//   - FNoticesWritten, recorded by writeNotice only after a write succeeds, so
//     the cost is visible in the same ledger as every other notice;
//   - the operator typing-hold, so a line is never submitted into a half-typed
//     prompt in the bubble the operator is currently dived into;
//   - InputReady/deliverWhenReadyThen, so a session still on the resume menu
//     doesn't swallow the line unsubmitted.
//
// It uses k.session, NEVER EnsureAlive: a cold bubble is left cold. Waking one
// to hand it a system instruction pays the full prompt-cache rewarm, which for
// the context pump in particular would cost more than the problem it reports.
func (k *Kernel) SystemNotice(a addr.Address, text string) bool {
	if a == "" || a.IsRoot() || text == "" {
		return false
	}
	if k.isFocused(a) && k.typingActive() {
		return false // don't submit the operator's half-typed line
	}
	s := k.session(a)
	if s == nil || !s.Alive() {
		return false // cold or dead: not worth a rewarm, and nothing to write to
	}
	d := k.Notify.System(a, text, time.Now())
	if d.Action == notify.Suppress {
		// A suppression no counter records is indistinguishable from a lost
		// write, so the ceiling's denial is metered like every other one.
		if d.Capped {
			k.Cost.Add(a, costmeter.FNoticesCapped, 1)
		}
		return false
	}
	// No announced level is claimed and none is unclaimed on failure: this
	// decision carries Announce 0 because it announces no backlog.
	return k.writeNotice(a, s, d)
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

// nudgeRecovery is how long an already-announced backlog can sit before a sweep
// re-announces it — the safety net for a notice that never landed (a cold bubble
// that stalled booting, a lost write). Well under the drain interval.
const nudgeRecovery = 2 * time.Minute

// decideRecovery is decide's sweep-side twin: same critical section, same
// policy, same claim. RecoverUnread used to make this call itself (recoverNudge
// + a hand-formatted drain line), which meant the fleet had TWO places that
// could decide to announce a backlog — the send path and the 45s sweep — and
// nothing stopping both from doing it for the same messages.
//
// The staleness question is answered here rather than in the policy because
// only the kernel knows when it last wrote: it is keyed off lastNudge, NOT off
// notified == 0. Those are different questions. The announced count is INV-2's
// high-water and legitimately falls to zero when a delivery consumes the
// backlog it announced, which does not mean nothing was ever written; reading
// it as "never announced" made every inlined delivery trigger a redundant drain
// nudge.
//
// Notifiable, not unread: a bubble whose entire backlog is mute-suppressed must
// stay cold. UnreadCount counts muted messages (correctly — they are unread and
// inbox() still shows them), so keying the sweep off it paged in exactly the
// bubbles muting exists to leave alone, paying the full prompt-cache rewarm for
// traffic already declared to be noise.
// State carries only Notifiable and Announced: Recover consults nothing else,
// because whether this pass may page a cold bubble in is the sweep's own
// question (hotOnly) and is answered by RecoverUnread.
func (k *Kernel) decideRecovery(a addr.Address) (d notify.Decision, prev int, proceed bool) {
	now := time.Now()
	k.notifyMu.Lock()
	prev = k.notified[a]
	last := k.lastNudge[a]
	n := k.Store.NotifiableCount(a)
	d = k.Notify.Recover(a, notify.State{
		Notifiable: n,
		Announced:  prev,
	}, last.IsZero() || now.Sub(last) >= nudgeRecovery, now)
	if d.Action != notify.Suppress {
		// Claimed under the same lock as the read, so a concurrent send-path
		// delivery sees the claim rather than a stale zero.
		k.notified[a] = d.Announce
		k.lastNudge[a] = time.Now()
	}
	k.notifyMu.Unlock()

	if d.Action == notify.Suppress {
		switch {
		case n == 0: // nothing to announce is not a suppression; there was no notice to spend
		case d.Capped:
			k.Cost.Add(a, costmeter.FNoticesCapped, 1)
		default:
			k.Cost.Add(a, costmeter.FNoticesSuppressed, 1)
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
// to deliverWhenReadyThen off the caller's path — the same discipline the send
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
	// NOTE the strength of this `true`: it means "accepted for delivery", not
	// "written". deliverWhenReadyThen can still time out or find a dead session
	// and return without writing, in which case onWritten never runs. That is
	// correct for the counters (FNoticesWritten records only real writes) but
	// callers that treat a true return as proof of delivery are claiming more
	// than this reports -- see the "claimed only on success" caveat on
	// cmd/bubbles/contextpump.go's markPumped, whose throttle window can be
	// spent on this branch for a notice that never landed.
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
