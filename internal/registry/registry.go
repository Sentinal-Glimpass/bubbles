// Package registry tracks all bubbles and assigns child addresses.
package registry

import (
	"strconv"
	"strings"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

type Status string

const (
	Idle    Status = "idle"
	Working Status = "working"
	Waiting Status = "waiting"
	Done    Status = "done"
)

// Bubble is the live state of one agent in the fleet.
type Bubble struct {
	Addr      addr.Address
	Persona   string
	Status    Status
	Parent    addr.Address
	Dir       string
	SessionID string // claude session id, so same-folder bubbles resume distinctly
}

// Registry is the in-memory fleet state.
type Registry struct {
	mu      sync.Mutex
	bubbles map[addr.Address]*Bubble
	nextSeq map[addr.Address]int
}

// New returns a Registry pre-seeded with the root bubble.
func New() *Registry {
	r := &Registry{
		bubbles: map[addr.Address]*Bubble{},
		nextSeq: map[addr.Address]int{},
	}
	r.bubbles[addr.Root] = &Bubble{Addr: addr.Root, Persona: "root", Status: Idle}
	return r
}

// Add creates a child bubble under parent and returns it.
func (r *Registry) Add(parent addr.Address, persona, dir string) *Bubble {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSeq[parent]++
	child := parent.Child(strconv.Itoa(r.nextSeq[parent]))
	b := &Bubble{Addr: child, Persona: persona, Status: Working, Parent: parent, Dir: dir}
	r.bubbles[child] = b
	return b
}

func (r *Registry) Get(a addr.Address) (*Bubble, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bubbles[a]
	return b, ok
}

func (r *Registry) SetStatus(a addr.Address, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Status = s
	}
}

// All returns every bubble, including root (unordered).
func (r *Registry) All() []*Bubble {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Bubble, 0, len(r.bubbles))
	for _, b := range r.bubbles {
		out = append(out, b)
	}
	return out
}

// Restore inserts a bubble with an explicit address (used when rehydrating a
// saved fleet) and advances the parent's child counter so later spawns don't
// reuse an address.
func (r *Registry) Restore(b Bubble) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nb := b
	r.bubbles[b.Addr] = &nb
	if i := lastSegInt(b.Addr); i > r.nextSeq[b.Parent] {
		r.nextSeq[b.Parent] = i
	}
}

// lastSegInt returns the integer value of an address's final segment ("0.1.2"->2).
func lastSegInt(a addr.Address) int {
	s := string(a)
	if i := strings.LastIndex(s, "."); i >= 0 {
		n, _ := strconv.Atoi(s[i+1:])
		return n
	}
	return 0
}

// Children returns the direct children of a (unordered).
func (r *Registry) Children(a addr.Address) []*Bubble {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*Bubble{}
	for _, b := range r.bubbles {
		if b.Parent == a {
			out = append(out, b)
		}
	}
	return out
}
