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

// maxCoalesceIDs bounds the ids remembered per coalescing window. The count
// keeps growing past this, so the summary stays truthful while the buffer
// stays O(1) under a flood.
const maxCoalesceIDs = 64

// Action is what the caller should do with a message's notification. It says
// nothing about storing the message: the message is always stored, so no
// message is ever dropped by this engine.
type Action int

const (
	Suppress Action = iota // file silently: no wake, no notice
	Notice                 // write the "you have mail" notice
	Inline                 // write the notice WITH bodies inlined; those ids have spent their notification
	Rollup                 // write an "N× subject since T" summary
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

// muteWindow tracks one open mute window for a (bubble, rule) pair: when it
// opened and how many messages it has swallowed since, which is exactly the
// material the expiry rollup reports.
type muteWindow struct {
	opened  time.Time
	count   int
	source  string
	subject string
}

// coalesceBuf batches non-urgent follow-ups so a burst costs one notice
// instead of one per message.
type coalesceBuf struct {
	opened  time.Time
	count   int
	ids     []int
	last    Message
	backlog int // recipient's Notifiable at the time of the last buffered message
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

	// 1. Mute. This runs before any urgency handling: an urgent muted message
	// must not page in a cold bubble, because that costs a full prompt-cache
	// rewarm for traffic the bubble has already declared to be noise.
	if rs := p.rules(to); rs != nil {
		if r, ok := rs.Match(msg.Source, msg.Subject, msg.Body); ok {
			w := b.windows[r.ID]
			switch {
			case w == nil:
				// First match opens the window; this message still delivers,
				// so the bubble learns the traffic exists at least once.
				b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
			case now.Sub(w.opened) < r.Window:
				w.count++
				w.source, w.subject = msg.label(), msg.Subject
				return Decision{Action: Suppress, MarkMuted: true, Wake: false, Rule: r.ID}
			default:
				// Window expired: report what was swallowed, then reopen.
				n, since, src, subj := w.count, w.opened, w.source, w.subject
				if n == 0 {
					// Nothing accumulated; reopen and deliver normally.
					b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
					break
				}
				// The ceiling still applies: a rollup is a real write. It is
				// checked BEFORE the window is reopened, because the swallowed
				// messages carry MarkMuted and are therefore not notifiable in
				// the store -- resetting the count on a capped rollup would
				// erase the only remaining record of them, which is the
				// silent-stall failure direction.
				if !p.ceiling.Allow(to, now) {
					return Decision{Action: Suppress, Rule: r.ID, Capped: true}
				}
				// The message that triggers the rollup is deliberately neither
				// delivered on its own nor counted in the rollup it triggered:
				// it is the first message of the window it opens here, exactly
				// like the first match that opened the previous one.
				b.windows[r.ID] = &muteWindow{opened: now, source: msg.label(), subject: msg.Subject}
				return Decision{
					Action: Rollup,
					Text:   renderRollup(n, subj, src, since),
					Wake:   false, // a rollup is by definition not worth a wake
					Rule:   r.ID,
					// The message that triggered the rollup is not delivered
					// and stays notifiable, so the backlog it belongs to is
					// what remains announced.
					Announce: st.Notifiable,
				}
			}
		}
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
	if st.Announced > 0 {
		return Decision{Action: Suppress}
	}

	// 3. Coalesce. Urgent mail bypasses entirely. A first message delivers
	// and opens the window; its non-urgent followers inside the window are
	// buffered and drained by Pending.
	if !msg.Urgent {
		if c := b.coalesce; c != nil && now.Sub(c.opened) < CoalesceWindow {
			c.count++
			if len(c.ids) < maxCoalesceIDs {
				c.ids = append(c.ids, msg.ID)
			}
			c.last = msg
			c.backlog = st.Notifiable
			return Decision{Action: Suppress}
		}
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

// Pending drains to's coalescing window if it has expired, returning the
// decision the caller should act on. The second result is false when there is
// nothing to write, which is the common case. When it is false because the
// INV-1 ceiling denied the write (rather than because nothing was due), the
// returned Decision has Capped set so the caller can meter it.
func (p *Policy) Pending(to addr.Address, now time.Time) (Decision, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	b := p.st[to]
	if b == nil || b.coalesce == nil {
		return Decision{}, false
	}
	c := b.coalesce
	if now.Sub(c.opened) < CoalesceWindow {
		return Decision{}, false
	}
	b.coalesce = nil
	if c.count == 0 {
		// The window opened on a message that was delivered immediately and
		// never gained siblings: nothing left to say.
		return Decision{}, false
	}
	if !p.ceiling.Allow(to, now) {
		// Dropping the notice is safe: the messages remain notifiable in the
		// store, so a later Decide will announce them. Capped is reported even
		// though ok is false, so the caller can tell this apart from "nothing
		// due" and record it: a suppression no counter records is
		// indistinguishable from a message the system lost.
		return Decision{Action: Suppress, Capped: true}, false
	}

	if c.count == 1 {
		d := deliver(c.last, State{Notifiable: c.backlog, Hot: true}, false)
		d.IDs = c.ids
		return d, true
	}
	return Decision{
		Action:   Rollup,
		Text:     renderRollup(c.count, c.last.Subject, c.last.label(), c.opened),
		IDs:      c.ids,
		Announce: c.backlog,
	}, true
}

// Clear drops to's coalescing buffer and mute windows, and is called when the
// bubble reads its inbox: a drained backlog makes a pending batch summary
// stale, and the next arrival deserves a fresh announcement. INV-2 needs no
// reset here because it reads the store's count, not anything local.
func (p *Policy) Clear(to addr.Address) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b := p.st[to]; b != nil {
		b.coalesce = nil
		b.windows = map[string]*muteWindow{}
	}
}
