package notify

import (
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Delivery economics. A bubble's attention is the scarce resource: every
// notice costs the recipient a turn, and every cold-bubble wake costs a full
// prompt-cache rewarm. These bounds keep an inlined message cheaper than the
// inbox() round-trip it replaces, and stop inlining from turning a backlog
// into a wall of text.
const (
	// InlineMaxBytes is the largest sanitised body that may be typed into a
	// recipient's session instead of being read via inbox().
	InlineMaxBytes = 280
	// InlineMaxBacklog is the deepest backlog that still justifies inlining;
	// past it the recipient should read the whole queue at once.
	InlineMaxBacklog = 3
	// CoalesceWindow is how long a non-urgent follow-up waits to be batched
	// with its siblings rather than spending another notice.
	CoalesceWindow = 3 * time.Second
)

// Action is what the caller should do with a message's notification. It says
// nothing about storing the message: the message is always stored, so no
// message is ever dropped by this engine.
type Action int

const (
	Suppress Action = iota // file silently: no wake, no notice
	Notice                 // write the "you have mail" notice
	Inline                 // write the notice WITH bodies inlined; those ids have spent their notification
	Rollup                 // write an "N× subject since T" summary
	// System is a kernel-originated instruction to the bubble itself (e.g. the
	// context pump's "you are large, compact"). It is deliberately a distinct
	// action from Notice: Notice means "you have mail", and the caller meters
	// it as a delivery that replaced an inbox() round-trip. A System line
	// delivers no message and replaces no tool call, so counting it as one
	// would inflate exactly the efficiency number this phase is judged on.
	System
)

// String makes failing table tests readable.
func (a Action) String() string {
	switch a {
	case Suppress:
		return "Suppress"
	case Notice:
		return "Notice"
	case Inline:
		return "Inline"
	case Rollup:
		return "Rollup"
	case System:
		return "System"
	}
	return "Action(?)"
}

// Message is the notification-relevant projection of a stored message.
//
// Source and Display are separate on purpose. Source is the MATCH KEY that
// mute rules are written against, so it must be the stable, predictable
// identifier a bubble would name in a rule -- the webhook source ("pump"), or
// the sender's address ("0.1"). Display is the human label shown in the
// notice, which decorates that with the sender's current persona name
// ("0.1 (scout)"). Matching on the decorated string would mean a rule silently
// stops matching the day someone renames the sender, and would make the
// obvious rule (Source: "pump") never fire at all.
type Message struct {
	ID      int
	Source  string // match key for mute rules: webhook source, or sender address
	Display string // label rendered into the notice; falls back to Source when empty
	Subject string
	Body    string
	Urgent  bool
}

// label returns what the notice should show for this sender.
func (m Message) label() string {
	if m.Display != "" {
		return m.Display
	}
	return m.Source
}

// State is the recipient's situation at decision time, supplied by the
// caller. Announced is reconciled from the message store rather than from
// live session state, so a backlog that outlives a session is still
// announced exactly once.
type State struct {
	Notifiable int  // Store.NotifiableCount(to)
	Announced  int  // announced-and-unconsumed backlog; > 0 means a notice is outstanding (INV-2)
	Hot        bool // recipient has a live session
	// AlwaysOn marks a bubble that is contracted to stay reachable, and so
	// may be woken for non-urgent mail. The kernel populates it from the
	// registry's always-on flag.
	AlwaysOn bool
}

// Decision is the caller's instruction sheet. Text is always PTY-safe and
// single-line, so the caller may type it verbatim.
//
// MarkRead and IDs answer different questions and must not be conflated.
// MarkRead is "these messages were delivered in full, they have spent their
// notification"; it is only ever populated by an Inline delivery. The kernel
// records that by marking them non-notifiable, NOT by marking them read --
// read state belongs to the recipient's own inbox() call, and flipping it
// here would drop the message out of UnreadCount and out of inbox(), i.e.
// consume the recipient's mail on its behalf. IDs is "this decision covers
// these queued messages", populated when Pending drains a coalescing batch,
// and says nothing about whether they were read. They overlap in exactly one
// case: a drained batch of one that was small enough to inline, where the
// single id appears in both because it was both covered and delivered.
type Decision struct {
	Action    Action
	Text      string // rendered, PTY-safe, single line; empty unless Notice/Inline/Rollup
	MarkRead  []int  // message ids consumed by an Inline delivery
	IDs       []int  // message ids this decision covers (a drained coalescing batch)
	MarkMuted bool   // caller must call Store.SetMuted for this message
	Wake      bool   // caller may page in a cold bubble (false => never wake)
	Rule      string // id of the matching mute rule, "" if none
	// Capped records that this Suppress was the INV-1 flood ceiling denying a
	// write, as opposed to mute/dedup/coalescing. The caller cannot infer this
	// from Action alone, and it must be able to: a capped notice and a
	// coalesced one have very different cost meanings, and a suppression that
	// no counter records at all is indistinguishable from a bug.
	Capped bool
	// Announce is the notifiable backlog the caller must record as its
	// announced high-water AFTER acting on this decision -- which is not the
	// same as the backlog the decision was made against.
	//
	// INV-2 compares a high-water count against a set that shrinks underneath
	// it: an inlined message leaves the notifiable set the moment it is
	// delivered. Recording the pre-delivery count would leave the high-water
	// stranded above the remaining backlog, and the NEXT genuine message would
	// be deduped away against a backlog that no longer exists -- a silent
	// stall until the stale-notice sweep, caused by an unrelated short message
	// arriving just before it. So Inline reports what REMAINS notifiable.
	//
	// The rule lives here rather than in the caller because it is a property
	// of the decision, not of the store: only this package knows which
	// messages a decision consumed.
	Announce int
}

// bubbleState is all per-recipient policy state. It is only ever touched
// under Policy.mu. Note that it deliberately holds no announced counter: the
// store-derived State.Announced is the single source of truth for INV-2, and
// a policy-local copy could drift above it and reintroduce the silent-stall
// half of the 632fe95 oscillation.
type bubbleState struct {
	windows  map[string]*muteWindow // by rule id
	coalesce *coalesceBuf
}

// Policy decides, for each inbound message, whether it earns a notification.
// It is pure with respect to the outside world: no PTY, no store, no clock —
// time is always supplied by the caller so behaviour is deterministic and
// table-testable. The logic it replaces was fused to a PTY write and caused
// the 632fe95 flood precisely because it could not be tested.
type Policy struct {
	mu      sync.Mutex
	rules   func(addr.Address) *RuleSet
	ceiling *Ceiling
	st      map[addr.Address]*bubbleState
}

// NewPolicy returns a Policy that resolves mute rules with rules and enforces
// ceiling. rules may return nil for a bubble with no rules.
func NewPolicy(rules func(addr.Address) *RuleSet, ceiling *Ceiling) *Policy {
	return &Policy{
		rules:   rules,
		ceiling: ceiling,
		st:      map[addr.Address]*bubbleState{},
	}
}

// bubble returns to's state, creating it. Caller must hold p.mu.
func (p *Policy) bubble(to addr.Address) *bubbleState {
	b := p.st[to]
	if b == nil {
		b = &bubbleState{windows: map[string]*muteWindow{}}
		p.st[to] = b
	}
	return b
}

// Decide returns what the caller should do about msg arriving for to at now.
//
// The whole decision, including the state updates it implies, happens under a
// single lock acquisition. Decide is called concurrently from the send path
// and from a background sweep, and a check-then-act split here is exactly the
// double-notification race this engine exists to eliminate.
//
// Order is load-bearing:
//
//  1. mute     — evaluated first so it can veto the wake even for urgent mail
//  2. INV-2    — never re-announce a backlog that was already announced
//  3. coalesce — batch non-urgent followers; urgent bypasses
//  4. INV-1    — the flood ceiling, the last gate before any write
//  5. inline vs notice
//
// The ceiling is deliberately last: INV-1 caps notices *written*, so spending
// a token on a message that coalescing then suppresses would drain the bucket
// on nothing and cap a later genuine notice. The one exception is the mute
// rollup below, which is itself a write and so spends its token where it is.
func (p *Policy) Decide(to addr.Address, msg Message, st State, now time.Time) Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.bubble(to)

	// 1. Mute (mute.go), including TTL expiry and the window/rollup handling.
	if d, ok := p.mute(to, b, msg, st, now); ok {
		return d
	}

	// 2. INV-2 dedup. One notice per backlog: while ANY announcement is
	// outstanding and unconsumed, further arrivals are silent, because the
	// recipient will drain them all in the one inbox() call the standing
	// notice already asked for.
	//
	// This is a state test, not a growth test. Comparing counts
	// (Notifiable > Announced -> announce) would re-announce every time the
	// backlog grew, which costs the recipient a turn per message and is the
	// exact cost regression this phase exists to remove -- a 1/minute trickle
	// would spend a turn a minute forever. INV-1 and coalescing bound the
	// rate but cannot fix the shape.
	//
	// It holds across a relaunch: an unannounced backlog is still announced
	// (no silent stall), and an announced one is never re-announced (the
	// 632fe95 flood direction). It does not stall a consumed backlog either,
	// because Announce reports what REMAINS after a delivery -- an inlined
	// message drops the level back to 0 and the next arrival is announced.
	//
	// COUPLING (load-bearing, see mute.go's Rollup): a mute rollup sets
	// Announce = st.Notifiable while its trigger message is neither delivered
	// nor marked muted, so this gate then suppresses that trigger and
	// everything after it until the backlog is consumed. That is only safe
	// because renderRollup's text ends with "call inbox() to read." -- the
	// standing instruction is what covers the suppressed messages. If that
	// wording ever loses its inbox() instruction, this gate turns a rollup into
	// a permanent stall.
	if st.Announced > 0 {
		return Decision{Action: Suppress}
	}

	// 3. Coalesce (coalesce.go). Urgent mail bypasses entirely. A first message
	// delivers and opens the window; its non-urgent followers inside the window
	// are buffered and drained by Pending.
	if d, ok := p.coalesce(b, msg, st, now); ok {
		return d
	}

	// 4. INV-1 ceiling, last gate before the write. Nothing above may bypass
	// it; the caller records this as NoticesCapped.
	if !p.ceiling.Allow(to, now) {
		return Decision{Action: Suppress, Capped: true}
	}

	// Only a message that is actually written earns a coalescing window. If
	// this opened above the ceiling gate, a capped message would buy 3s of
	// silence it never paid for, and its own id would appear in neither the
	// batch's IDs nor any MarkRead -- silently dropped from both.
	if !msg.Urgent {
		b.coalesce = &coalesceBuf{opened: now}
	}

	return deliver(msg, st, !st.Hot && (msg.Urgent || st.AlwaysOn))
}

// deliver renders the terminal Inline-or-Notice choice for a message that has
// already passed every suppression gate.
func deliver(msg Message, st State, wake bool) Decision {
	clean := Sanitize(flatten(msg.Body))
	if len(clean) <= InlineMaxBytes && st.Notifiable <= InlineMaxBacklog {
		return Decision{
			Action:   Inline,
			Text:     renderInline(msg.label(), msg.Subject, clean, st.Notifiable),
			MarkRead: []int{msg.ID},
			Wake:     wake,
			// This message is delivered in full and leaves the notifiable set,
			// so the high-water must come down with it.
			Announce: st.Notifiable - 1,
		}
	}
	return Decision{
		Action:   Notice,
		Text:     renderNotice(msg.label(), msg.Subject, st.Notifiable),
		Wake:     wake,
		Announce: st.Notifiable,
	}
}

// System renders a kernel-originated instruction to the bubble itself, subject
// to the one gate that applies to it: INV-1.
//
// It exists so that no caller ever has a reason to write to a session
// directly. A raw PTY write is invisible to the ceiling, invisible to the cost
// meter, and invisible to the operator typing-hold -- which is precisely the
// shape of the 632fe95 flood, and a background sweep on a 2-minute ticker is
// exactly the kind of caller that reintroduces it.
//
// The mail-shaped gates are deliberately NOT applied. Mute rules match on a
// sender and there is no sender here; INV-2 dedups an announced backlog and
// this announces none; coalescing batches followers of a message that does not
// exist. Applying them would silence a system instruction for reasons that have
// nothing to do with it -- e.g. a bubble with an outstanding mail notice could
// never be told to compact. The ceiling still bounds the rate absolutely, and
// the caller is expected to throttle on top of it for its own cadence.
func (p *Policy) System(to addr.Address, text string, now time.Time) Decision {
	clean := Sanitize(flatten(text))
	if clean == "" {
		return Decision{Action: Suppress}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ceiling.Allow(to, now) {
		return Decision{Action: Suppress, Capped: true}
	}
	// Announce stays 0: the announced high-water is INV-2 state about MAIL, and
	// claiming a level here would dedup away the next genuine message.
	return Decision{Action: System, Text: clean}
}

// Recover is the SWEEP half of the same decision Decide makes for a freshly
// filed message: it answers "does this standing backlog still need to be
// announced?". Both halves live here so the send path and the periodic sweep
// reach one decision point and cannot both announce the same backlog -- the
// double-notice observed live as two notices for one webhook event.
//
// It takes no Message because a sweep has none: the backlog is whatever is
// left notifiable, and there is nothing new to mute-match, inline or coalesce.
// It deliberately DOES apply the two gates that bound cost:
//
//   - INV-2, so an announced-and-unconsumed backlog is not re-announced. stale
//     overrides it: that is the safety net for a notice that never landed (a
//     bubble that stalled booting, a lost write), and it is keyed by the
//     caller off WHEN it last wrote, not off the announced count -- the count
//     legitimately falls to zero when a delivery consumes the backlog it
//     announced, which does not mean nothing was ever written.
//   - INV-1, because a drain line is a real write. This is the ceiling that
//     bounds the 632fe95 direction: a backlog whose notice keeps failing to
//     land cannot re-emit on every sweep forever.
//
// Notifiable is the caller's NotifiableCount, never its UnreadCount: a backlog
// of nothing but mute-suppressed traffic must never wake anything, or the
// sweep pays exactly the prompt-cache rewarm that muting exists to prevent.
//
// Only Notifiable and Announced are consulted; Hot and AlwaysOn are not, and
// the returned Decision carries no Wake. Reachability is the SWEEP's question,
// not the policy's: the caller already splits hot-only from full passes and
// knows which of the two it is running, so a Wake here would either duplicate
// that split or silently contradict it. The mute veto that actually prevents a
// wake has already been applied at Decide time — a muted message is not
// notifiable, so it never reaches this function's Notifiable count at all.
func (p *Policy) Recover(to addr.Address, st State, stale bool, now time.Time) Decision {
	p.mu.Lock()
	defer p.mu.Unlock()

	if st.Notifiable == 0 {
		return Decision{Action: Suppress} // nothing to say; not a suppression
	}
	if st.Announced > 0 && !stale {
		return Decision{Action: Suppress}
	}
	if !p.ceiling.Allow(to, now) {
		return Decision{Action: Suppress, Capped: true}
	}
	return Decision{
		Action:   Notice,
		Text:     RenderDrain(st.Notifiable),
		Announce: st.Notifiable,
	}
}

