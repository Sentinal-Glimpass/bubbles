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
	all []*Message
}

func New() *Store { return &Store{} }

// Append files a message and returns its assigned ID. If it is a reply, the
// referenced message is marked Replied.
func (s *Store) Append(m Message) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
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
