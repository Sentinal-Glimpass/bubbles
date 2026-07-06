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
	Name      string // display name (preferred); falls back to Persona when empty
	Persona   string // legacy label (pre-Name); kept for backward compatibility
	Status    Status
	Parent    addr.Address
	Dir       string
	Model     string // claude --model alias ("" => default); persisted so a restart keeps it
	Goal      string // initial prompt/instruction, used on the bubble's first (lazy) launch
	SessionID string // claude session id; "" until first launched (lazy), then set so it resumes

	// WebhookToken is the secret in this bubble's incoming-webhook URL
	// (/w/<token>). "" until minted on first webhook() call; persisted so the
	// URL is stable across restarts. Rotating it revokes the old URL.
	WebhookToken string

	// Disabled parks a bubble: it's hidden from everyone's contacts and cannot be
	// launched (dive/message/schedule/webhook are all no-ops) until re-enabled.
	Disabled bool
}

// Label is what to show for a bubble: its Name, or its legacy Persona if Name is
// unset (so bubbles saved before the rename still display correctly).
func (b *Bubble) Label() string {
	if b.Name != "" {
		return b.Name
	}
	return b.Persona
}

// Registry is the in-memory fleet state.
type Registry struct {
	mu      sync.Mutex
	bubbles map[addr.Address]*Bubble
	nextSeq map[addr.Address]int
	version int64 // bumped on every structural/persisted change (add/remove/rename/remodel/regoal/restore)
}

// Version returns a counter that increments on every persisted change to the
// fleet. Callers snapshot it and re-save only when it moves, so agent-driven
// spawns/edits/deletes get persisted without polling the whole registry.
func (r *Registry) Version() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
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
	r.version++
	return b
}

// Remove deletes a bubble from the registry (used when deleting a group session).
func (r *Registry) Remove(a addr.Address) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.bubbles[a]; ok {
		delete(r.bubbles, a)
		r.version++
	}
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

// SetName renames a bubble (display + future launches).
func (r *Registry) SetName(a addr.Address, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Name = name
		r.version++
	}
}

// SetModel changes a bubble's model alias (applied on its next relaunch).
func (r *Registry) SetModel(a addr.Address, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Model = model
		r.version++
	}
}

// SetDisabled parks or un-parks a bubble.
func (r *Registry) SetDisabled(a addr.Address, disabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Disabled = disabled
		r.version++
	}
}

// SetWebhookToken sets (or rotates) a bubble's incoming-webhook secret.
func (r *Registry) SetWebhookToken(a addr.Address, tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.WebhookToken = tok
		r.version++
	}
}

// ByWebhookToken finds the bubble owning an incoming-webhook token.
func (r *Registry) ByWebhookToken(tok string) (*Bubble, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tok == "" {
		return nil, false
	}
	for _, b := range r.bubbles {
		if b.WebhookToken == tok {
			return b, true
		}
	}
	return nil, false
}

// SetGoal changes a bubble's initial instruction (used on its next fresh launch).
func (r *Registry) SetGoal(a addr.Address, goal string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Goal = goal
		r.version++
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
	r.version++
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
