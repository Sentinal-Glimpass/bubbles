// Package groups holds named, ad-hoc groupings of bubbles — arrangement only,
// independent of the spawn tree. A group may have a claude coordinator session
// attached, and can be deleted at any time without affecting anyone's contacts.
package groups

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Group is a named set of member bubbles, optionally with a coordinator session.
type Group struct {
	Name    string
	Members []addr.Address
	Session addr.Address // "" = no session attached
}

// Store holds all groups in creation order.
type Store struct {
	mu   sync.Mutex
	list []*Group
}

func New() *Store { return &Store{} }

// Create adds a group (members copied) and returns it.
func (s *Store) Create(name string, members []addr.Address) *Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &Group{Name: name, Members: append([]addr.Address(nil), members...)}
	s.list = append(s.list, g)
	return g
}

// Delete removes a group by name (no-op if absent).
func (s *Store) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list[:0]
	for _, g := range s.list {
		if g.Name != name {
			out = append(out, g)
		}
	}
	s.list = out
}

// AddMember adds a to the named group (no-op if absent or already a member).
func (s *Store) AddMember(name string, a addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.list {
		if g.Name != name {
			continue
		}
		for _, m := range g.Members {
			if m == a {
				return
			}
		}
		g.Members = append(g.Members, a)
		return
	}
}

// RemoveMember drops a from the named group (no-op if absent).
func (s *Store) RemoveMember(name string, a addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.list {
		if g.Name == name {
			g.Members = without(g.Members, a)
			return
		}
	}
}

// PurgeMember removes a from every group's membership (and clears it as a
// session), used when a bubble is deleted.
func (s *Store) PurgeMember(a addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.list {
		g.Members = without(g.Members, a)
		if g.Session == a {
			g.Session = ""
		}
	}
}

func without(ms []addr.Address, a addr.Address) []addr.Address {
	out := ms[:0]
	for _, m := range ms {
		if m != a {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) Get(name string) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.list {
		if g.Name == name {
			return *g, true
		}
	}
	return Group{}, false
}

// SetSession links a coordinator session address to a group.
func (s *Store) SetSession(name string, sess addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.list {
		if g.Name == name {
			g.Session = sess
		}
	}
}

func (s *Store) All() []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Group, 0, len(s.list))
	for _, g := range s.list {
		out = append(out, *g)
	}
	return out
}

// Tags returns the names of groups a belongs to (as a member or as the session).
func (s *Store) Tags(a addr.Address) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, g := range s.list {
		if g.Session == a {
			out = append(out, g.Name)
			continue
		}
		for _, m := range g.Members {
			if m == a {
				out = append(out, g.Name)
				break
			}
		}
	}
	return out
}
