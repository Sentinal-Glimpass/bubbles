// Package inbox is the message store: every send lands here, recipients read on
// their own schedule, and senders can check delivery/read/replied status.
package inbox

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Message is one piece of mail. ID is a monotonic sequence (also the thread key
// a reply references via ReplyTo).
type Message struct {
	ID       int
	From     addr.Address
	FromName string // sender persona, for human-readable display
	To       addr.Address
	Subject  string
	Body     string
	ReplyTo  int // id this message replies to (0 = not a reply)
	Read     bool
	Replied  bool // a reply referencing this message has been sent
}

// Store holds all messages in a flat log, queried by recipient or sender.
type Store struct {
	mu  sync.Mutex
	seq int
	ver int64 // bumped on every change, so the app persists only when needed
	all []*Message
}

func New() *Store { return &Store{} }

// Version returns a counter that increments on every change (append/read/reply),
// so a periodic saver can skip writing when nothing has moved.
func (s *Store) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ver
}

// Snapshot returns a copy of all messages and the current ID sequence, for
// persistence.
func (s *Store) Snapshot() ([]Message, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.all))
	for i, m := range s.all {
		out[i] = *m
	}
	return out, s.seq
}

// Load replaces the store's contents from a saved snapshot (used on restore), so
// unread messages survive a restart and the ID sequence continues (reply_to
// references still resolve).
func (s *Store) Load(msgs []Message, seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all = make([]*Message, len(msgs))
	for i := range msgs {
		cp := msgs[i]
		s.all[i] = &cp
		if cp.ID > seq {
			seq = cp.ID
		}
	}
	s.seq = seq
	s.ver++
}

// Append files a message and returns its assigned ID. If it is a reply, the
// referenced message is marked Replied.
func (s *Store) Append(m Message) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.ver++
	m.ID = s.seq
	cp := m
	s.all = append(s.all, &cp)
	if m.ReplyTo > 0 {
		for _, x := range s.all {
			if x.ID == m.ReplyTo {
				x.Replied = true
			}
		}
	}
	return m.ID
}

// Take returns owner's unread messages and marks them read.
func (s *Store) Take(owner addr.Address) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.all {
		if m.To == owner && !m.Read {
			m.Read = true
			out = append(out, *m)
		}
	}
	if len(out) > 0 {
		s.ver++
	}
	return out
}

// UnreadCount returns how many unread messages owner has.
func (s *Store) UnreadCount(owner addr.Address) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.all {
		if m.To == owner && !m.Read {
			n++
		}
	}
	return n
}

// Sent returns the messages from sent (for status checks), oldest first.
func (s *Store) Sent(from addr.Address) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.all {
		if m.From == from {
			out = append(out, *m)
		}
	}
	return out
}

// All returns owner's full received history (read and unread).
func (s *Store) All(owner addr.Address) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.all {
		if m.To == owner {
			out = append(out, *m)
		}
	}
	return out
}
