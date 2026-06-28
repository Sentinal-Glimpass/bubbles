// Package registry tracks all bubbles and assigns child addresses.
package registry

import (
	"strconv"
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
	Addr    addr.Address
	Persona string
	Status  Status
	Parent  addr.Address
	Dir     string
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
