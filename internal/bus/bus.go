// Package bus is an in-memory, synchronous, address-routed message bus.
package bus

import (
	"errors"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Message is a single piece of mail between bubbles.
type Message struct {
	From    addr.Address
	To      addr.Address
	Subject string
	Body    string
}

// Handler receives messages delivered to a subscribed address.
type Handler func(Message)

// ErrNoInbox is returned by Send when the destination has no subscriber.
var ErrNoInbox = errors.New("bus: no inbox for address")

// Bus routes messages to per-address handlers (one inbox per address).
type Bus struct {
	mu       sync.Mutex
	handlers map[addr.Address]Handler
}

func New() *Bus { return &Bus{handlers: map[addr.Address]Handler{}} }

// Subscribe sets the inbox handler for an address, replacing any existing one.
func (b *Bus) Subscribe(a addr.Address, h Handler) {
	b.mu.Lock()
	b.handlers[a] = h
	b.mu.Unlock()
}

func (b *Bus) Unsubscribe(a addr.Address) {
	b.mu.Lock()
	delete(b.handlers, a)
	b.mu.Unlock()
}

// Send routes m to m.To's handler, called outside the lock so a handler may
// itself Send. Returns ErrNoInbox if nothing is subscribed.
func (b *Bus) Send(m Message) error {
	b.mu.Lock()
	h, ok := b.handlers[m.To]
	b.mu.Unlock()
	if !ok {
		return ErrNoInbox
	}
	h(m)
	return nil
}
