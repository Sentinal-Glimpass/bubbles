// Package inbox is the message store: every send lands here, recipients read on
// their own schedule. No interruption.
package inbox

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Message is one piece of mail. ID is a monotonic sequence (also the thread key
// a reply can reference).
type Message struct {
	ID       int
	From     addr.Address
	FromName string // sender persona, for human-readable display
	To       addr.Address
	Subject  string
	Body     string
	Urgent   bool
	Read     bool
}

// Store holds per-recipient message lists.
type Store struct {
	mu   sync.Mutex
	seq  int
	msgs map[addr.Address][]*Message
}

func New() *Store { return &Store{msgs: map[addr.Address][]*Message{}} }

// Append files a message under its recipient and returns its assigned ID.
func (s *Store) Append(m Message) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	m.ID = s.seq
	cp := m
	s.msgs[m.To] = append(s.msgs[m.To], &cp)
	return m.ID
}

// Take returns owner's unread messages and marks them read.
func (s *Store) Take(owner addr.Address) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.msgs[owner] {
		if !m.Read {
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
	for _, m := range s.msgs[owner] {
		if !m.Read {
			n++
		}
	}
	return n
}

// All returns owner's full message history (read and unread).
func (s *Store) All(owner addr.Address) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.msgs[owner]))
	for _, m := range s.msgs[owner] {
		out = append(out, *m)
	}
	return out
}
