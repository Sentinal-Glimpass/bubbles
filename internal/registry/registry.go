// Package registry tracks all bubbles and assigns child addresses.
package registry

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
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

	// ControlToken is the secret in this bubble's CONTROL webhook URL
	// (/c/<token>), which executes fleet actions (spawn/delete/list) as this
	// bubble — minted only for spawn-granted bubbles. Kept separate from
	// WebhookToken so a shared message-webhook never carries control authority.
	ControlToken string

	// Disabled parks a bubble: it's hidden from everyone's contacts and cannot be
	// launched (dive/message/schedule/webhook are all no-ops) until re-enabled.
	Disabled bool

	// AlwaysOn marks an always-on RECEIVER: the kernel keeps it hot (exempt from
	// idle + budget eviction) and relaunches it if it dies, and every message to
	// it is treated as urgent. So an inbound webhook/message is delivered to a
	// live session immediately — no cold-wake to fail. For critical receivers
	// (e.g. a WhatsApp/OOF channel bubble) that must never miss a message.
	AlwaysOn bool

	// MuteRules are the mute predicates this bubble has declared for inbound
	// traffic (see package notify): a message matching one is noise and the
	// kernel stops waking this bubble for it. Persisted so a bubble that has
	// silenced a noisy pump stays silenced across restarts.
	MuteRules []notify.Rule
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

// SetAlwaysOn marks/unmarks a bubble as an always-on receiver.
func (r *Registry) SetAlwaysOn(a addr.Address, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.AlwaysOn = on
		r.version++
	}
}

// SetMuteRules replaces a bubble's mute rules (nil/empty clears them all).
func (r *Registry) SetMuteRules(a addr.Address, rules []notify.Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.MuteRules = append([]notify.Rule(nil), rules...)
		r.version++
	}
}

// MuteRules returns a copy of a bubble's mute rules, so a caller mutating the
// returned slice can't corrupt registry state.
func (r *Registry) MuteRules(a addr.Address) []notify.Rule {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		return append([]notify.Rule(nil), b.MuteRules...)
	}
	return nil
}

// ReapExpiredMuteRules drops every bubble's TTL-expired mute rules and returns
// how many rules it removed fleet-wide. Expired rules already stopped matching
// (notify.Compiled.Match enforces the TTL), so this changes no behaviour: it
// reclaims the memory and — the reason it matters — frees the notify.MaxRules
// quota, which counted rules that can never match again and so could stop a
// bubble from ever adding a new one.
//
// The read-modify-write happens inside ONE hold of r.mu, deliberately. Doing it
// as MuteRules() then SetMuteRules() from a sweep goroutine would race a
// concurrent mute() on the same bubble and silently discard the rule the bubble
// had just been told was accepted. No lock is held across anything unbounded
// here: filtering a slice of at most MaxRules values is pure computation.
func (r *Registry) ReapExpiredMuteRules(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, b := range r.bubbles {
		if len(b.MuteRules) == 0 {
			continue
		}
		kept, n := notify.ReapExpiredRules(b.MuteRules, now)
		if n == 0 {
			continue
		}
		b.MuteRules = kept
		total += n
		r.version++
	}
	return total
}

// AlwaysOnAddrs returns every always-on receiver (for keep-alive sweeps).
func (r *Registry) AlwaysOnAddrs() []addr.Address {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []addr.Address
	for _, b := range r.bubbles {
		if b.AlwaysOn && !b.Disabled {
			out = append(out, b.Addr)
		}
	}
	return out
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

// SetControlToken sets (or rotates) a bubble's control-webhook secret.
func (r *Registry) SetControlToken(a addr.Address, tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.ControlToken = tok
		r.version++
	}
}

// ByControlToken finds the bubble owning a control-webhook token.
func (r *Registry) ByControlToken(tok string) (*Bubble, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tok == "" {
		return nil, false
	}
	for _, b := range r.bubbles {
		if b.ControlToken == tok {
			return b, true
		}
	}
	return nil, false
}

// SetDir changes a bubble's working directory (used on its next launch).
//
// Unlike SetSessionID this DOES bump version, and the difference is deliberate.
// Dir is durable fleet state the operator set: if it changes, fleet.json is
// stale until it is re-saved. SessionID is refreshed by SyncSessionIDs on the
// way INTO a save, so bumping there would mark the fleet dirty as a side effect
// of saving it. Dir is never written from the persist path, so it has no such
// feedback loop.
func (r *Registry) SetDir(a addr.Address, dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.Dir = dir
		r.version++
	}
}

// Dir returns a bubble's working directory and whether the bubble exists.
// Callers that both test and use the directory must read it ONCE into a local,
// for the same reason as SessionID below: two reads can straddle a write and
// launch a bubble in a directory the registry no longer records.
func (r *Registry) Dir(a addr.Address) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		return b.Dir, true
	}
	return "", false
}

// SetSessionID records the claude session id a bubble should resume from. This
// field is written from the kernel's relaunch path (ensureAlive) and from the
// pre-persist sweep (SyncSessionIDs), which can run concurrently for the same
// address — so it must go through the mutex like every other mutator here.
//
// Deliberately does NOT bump version. SessionID is persisted, but it is
// refreshed on the way INTO a save (SyncSessionIDs runs immediately before
// saveFleet); bumping the version there would mark the fleet dirty as a side
// effect of saving it and make the change-driven autosave re-save forever.
func (r *Registry) SetSessionID(a addr.Address, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		b.SessionID = id
	}
}

// SessionID returns a bubble's stored session id and whether the bubble exists.
// Callers that use the id more than once must read it ONCE into a local: the
// value can change under them between two calls, and a launch decision made on
// two different ids resumes the wrong conversation (or none).
func (r *Registry) SessionID(a addr.Address) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bubbles[a]; ok {
		return b.SessionID, true
	}
	return "", false
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
