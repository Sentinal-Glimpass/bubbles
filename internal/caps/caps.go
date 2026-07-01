// Package caps holds per-bubble contacts and spawn budgets. Root is implicitly
// allowed to send to anyone and to spawn without limit.
package caps

import (
	"errors"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// ErrNoBudget is returned by ConsumeSpawn when no spawn budget remains.
var ErrNoBudget = errors.New("caps: no spawn budget")

// Store is the capability store.
type Store struct {
	mu       sync.Mutex
	contacts map[addr.Address]map[addr.Address]bool
	spawn    map[addr.Address]int // count budget (finite grants)
	depth    map[addr.Address]int // spawn-grant depth: levels of spawning this bubble may seed
}

func New() *Store {
	return &Store{
		contacts: map[addr.Address]map[addr.Address]bool{},
		spawn:    map[addr.Address]int{},
		depth:    map[addr.Address]int{},
	}
}

// AddContact lets owner send to contact (one direction).
func (s *Store) AddContact(owner, contact addr.Address) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contacts[owner] == nil {
		s.contacts[owner] = map[addr.Address]bool{}
	}
	s.contacts[owner][contact] = true
}

// Introduce makes a and b mutual contacts.
func (s *Store) Introduce(a, b addr.Address) {
	s.AddContact(a, b)
	s.AddContact(b, a)
}

// CanSend reports whether from may send to to. Root may send to anyone.
func (s *Store) CanSend(from, to addr.Address) bool {
	if from == addr.Root {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contacts[from][to]
}

// Contacts returns the addresses owner may send to (unordered).
func (s *Store) Contacts(owner addr.Address) []addr.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []addr.Address{}
	for c := range s.contacts[owner] {
		out = append(out, c)
	}
	return out
}

// GrantSpawn gives owner a spawn budget of n children.
func (s *Store) GrantSpawn(owner addr.Address, n int) {
	s.mu.Lock()
	s.spawn[owner] = n
	s.mu.Unlock()
}

// GrantSpawnDepth grants owner the spawn ability to a given depth: depth 1 means
// it may spawn but its children may not (the grant does not propagate). Root is
// implicitly unlimited.
func (s *Store) GrantSpawnDepth(owner addr.Address, depth int) {
	s.mu.Lock()
	s.depth[owner] = depth
	s.mu.Unlock()
}

// SpawnDepth returns owner's granted spawn depth (0 = none). Root is unlimited.
func (s *Store) SpawnDepth(owner addr.Address) int {
	if owner == addr.Root {
		return 1 << 30
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.depth[owner]
}

// CanSpawn reports whether owner may spawn. Root always may; otherwise it needs
// either a count budget or a spawn-depth grant.
func (s *Store) CanSpawn(owner addr.Address) bool {
	if owner == addr.Root {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawn[owner] > 0 || s.depth[owner] > 0
}

// ConsumeSpawn decrements owner's count budget. Root and depth-granted bubbles
// are unlimited (no decrement).
func (s *Store) ConsumeSpawn(owner addr.Address) error {
	if owner == addr.Root {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.depth[owner] > 0 {
		return nil // depth grant: unlimited count
	}
	if s.spawn[owner] <= 0 {
		return ErrNoBudget
	}
	s.spawn[owner]--
	return nil
}
