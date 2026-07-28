package notify

import (
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// coalesceBuf batches non-urgent follow-ups so a burst costs one notice
// instead of one per message.
type coalesceBuf struct {
	opened  time.Time
	count   int
	ids     []int
	last    Message
	backlog int // recipient's Notifiable at the time of the last buffered message
}

// maxCoalesceIDs bounds the ids remembered per coalescing window. The count
// keeps growing past this, so the summary stays truthful while the buffer
// stays O(1) under a flood.
const maxCoalesceIDs = 64

// coalesce is gate 3 of Decide: urgent mail bypasses entirely; a non-urgent
// follower that arrives inside an open window is buffered and later drained by
// Pending. It assumes p.mu is held. ok == false means "fall through and
// deliver".
func (p *Policy) coalesce(b *bubbleState, msg Message, st State, now time.Time) (Decision, bool) {
	if msg.Urgent {
		return Decision{}, false
	}
	c := b.coalesce
	if c == nil || now.Sub(c.opened) >= CoalesceWindow {
		return Decision{}, false
	}
	c.count++
	if len(c.ids) < maxCoalesceIDs {
		c.ids = append(c.ids, msg.ID)
	}
	c.last = msg
	c.backlog = st.Notifiable
	return Decision{Action: Suppress}, true
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

// Clear drops to's pending coalescing batch, and is called when the bubble
// reads its inbox: the batch summary describes messages the bubble has now
// already read, so writing it would spend a turn on a notice about nothing.
//
// It deliberately does NOT reset the mute windows. Reopening a window on a
// read would make the very next noise message deliver again (the first match
// of a window always delivers), so a bubble that checks its inbox regularly
// would pay a notice per check for traffic it explicitly declared as noise --
// precisely the cost this phase exists to remove.
//
// INV-2 needs no reset here either: it reads the store's count, not anything
// local. The kernel clears its own announced high-water alongside this call.
func (p *Policy) Clear(to addr.Address) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b := p.st[to]; b != nil {
		b.coalesce = nil
	}
}
