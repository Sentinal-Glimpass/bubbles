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
	Inline                 // write the notice WITH bodies inlined; mark those read
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
type Message struct {
	ID      int
	Source  string // sender label: bubble name, or webhook source
	Subject string
	Body    string
	Urgent  bool
}

// State is the recipient's situation at decision time, supplied by the
// caller. Announced is reconciled from the message store rather than from
// live session state, so a backlog that outlives a session is still
// announced exactly once.
type State struct {
	Notifiable int  // Store.NotifiableCount(to)
	Announced  int  // backlog size already announced (INV-2)
	Hot        bool // recipient has a live session
	AlwaysOn   bool
}

// Decision is the caller's instruction sheet. Text is always PTY-safe and
// single-line, so the caller may type it verbatim.
type Decision struct {
	Action    Action
	Text      string // rendered, PTY-safe, single line; empty unless Notice/Inline/Rollup
	MarkRead  []int  // message ids consumed by an Inline delivery
	IDs       []int  // message ids this decision covers (a drained coalescing batch)
	MarkMuted bool   // caller must call Store.SetMuted for this message
	Wake      bool   // caller may page in a cold bubble (false => never wake)
	Rule      string // id of the matching mute rule, "" if none
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
				b.windows[r.ID] = &muteWindow{opened: now, source: msg.Source, subject: msg.Subject}
			case now.Sub(w.opened) < r.Window:
				w.count++
				w.source, w.subject = msg.Source, msg.Subject
				return Decision{Action: Suppress, MarkMuted: true, Wake: false, Rule: r.ID}
			default:
				// Window expired: report what was swallowed, then reopen.
				n, since, src, subj := w.count, w.opened, w.source, w.subject
				b.windows[r.ID] = &muteWindow{opened: now, source: msg.Source, subject: msg.Subject}
				if n == 0 {
					break // nothing accumulated; fall through to normal delivery
				}
				// The ceiling still applies: a rollup is a real write.
				if !p.ceiling.Allow(to, now) {
					return Decision{Action: Suppress, Rule: r.ID}
				}
				return Decision{
					Action: Rollup,
					Text:   renderRollup(n, subj, src, since),
					Wake:   false, // a rollup is by definition not worth a wake
					Rule:   r.ID,
				}
			}
		}
	}

	// 2. INV-2 dedup, against the store-derived count only. This holds across
	// a relaunch: an unannounced backlog is still announced (no silent
	// stall), and an announced one is never re-announced (the 632fe95 flood
	// direction).
	if st.Notifiable <= st.Announced {
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
		b.coalesce = &coalesceBuf{opened: now}
	}

	// 4. INV-1 ceiling, last gate before the write. Nothing above may bypass
	// it; the caller records this as NoticesCapped.
	if !p.ceiling.Allow(to, now) {
		return Decision{Action: Suppress}
	}

	return deliver(msg, st, !st.Hot && (msg.Urgent || st.AlwaysOn))
}

// deliver renders the terminal Inline-or-Notice choice for a message that has
// already passed every suppression gate.
func deliver(msg Message, st State, wake bool) Decision {
	clean := sanitize(flatten(msg.Body))
	if len(clean) <= InlineMaxBytes && st.Notifiable <= InlineMaxBacklog {
		return Decision{
			Action:   Inline,
			Text:     renderInline(msg.Source, msg.Subject, clean, st.Notifiable),
			MarkRead: []int{msg.ID},
			Wake:     wake,
		}
	}
	return Decision{
		Action: Notice,
		Text:   renderNotice(msg.Source, msg.Subject, st.Notifiable),
		Wake:   wake,
	}
}

// Pending drains to's coalescing window if it has expired, returning the
// decision the caller should act on. The second result is false when there is
// nothing to write, which is the common case.
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
		// store, so a later Decide will announce them.
		return Decision{}, false
	}

	if c.count == 1 {
		d := deliver(c.last, State{Notifiable: c.backlog, Hot: true}, false)
		d.IDs = c.ids
		return d, true
	}
	return Decision{
		Action: Rollup,
		Text:   renderRollup(c.count, c.last.Subject, c.last.Source, c.opened),
		IDs:    c.ids,
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
