// Package kernel wires the bus, capabilities, registry, and a runner into the
// four fleet operations: Send, Contacts, Introduce, Spawn.
package kernel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/caps"
	"github.com/Sentinal-Glimpass/bubbles/internal/costmeter"
	"github.com/Sentinal-Glimpass/bubbles/internal/groups"
	"github.com/Sentinal-Glimpass/bubbles/internal/inbox"
	"github.com/Sentinal-Glimpass/bubbles/internal/notify"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/sched"
	"github.com/Sentinal-Glimpass/bubbles/internal/tasks"
)

// ErrNotContact is returned by Send when from may not message to.
var ErrNotContact = errors.New("kernel: recipient not in contacts")

// ErrDisabled is returned by Send when the target bubble is disabled.
var ErrDisabled = errors.New("kernel: target bubble is disabled")

// ErrNotAllowed is returned for root-only actions attempted by non-root.
var ErrNotAllowed = errors.New("kernel: action not permitted")

// Kernel is the fleet engine.
type Kernel struct {
	Bus    *bus.Bus
	Caps   *caps.Store
	Reg    *registry.Registry
	Store  *inbox.Store
	Groups *groups.Store
	Sched  *sched.Store
	Tasks  *tasks.Store

	// Notify decides, for each inbound message, whether it earns a notice and
	// whether it may wake a cold recipient. Mute is evaluated here BEFORE any
	// page-in, which is what stops noisy traffic from paying a prompt-cache
	// rewarm.
	Notify *notify.Policy

	// Cost is the per-bubble cost/efficiency tally. Without it a notification
	// win and a notification regression look identical from the outside.
	Cost *costmeter.Meter

	// VerifierReap, when set (tests), replaces the delayed post-verdict deletion
	// of a task's verifier bubble with an inline call.
	VerifierReap func(addr.Address)

	// BrainBase is where per-bubble private brain folders live (keyed by
	// address). "" = brain seeding off.
	BrainBase string

	// DecisionsPath is the fleet's shared decisions ledger, appended through
	// LogDecision only (kernel-serialized). "" = ledger off.
	DecisionsPath string
	decisionsMu   sync.Mutex

	// RelaunchProbe is how long EnsureAlive waits to see if a *resumed* session
	// survives before falling back to a fresh one (a doomed --resume exits fast).
	// Tests set it to 0; real use gives claude a moment to fail.
	RelaunchProbe time.Duration

	// CurrentSessionID, if set, returns the LIVE session id a bubble is on now
	// (recorded by its session hook), which can differ from the launch id after an
	// in-session /resume. nil = just use the stored id.
	CurrentSessionID func(addr.Address) string

	// MemBudget is the total resident-RAM budget (bytes) for live worker
	// sessions. Sessions are packed by their ACTUAL memory — a 200 MB session
	// costs 200 MB, not a fixed slot — and when the sum exceeds the budget the
	// least-recently-used bubbles page out until it fits. 0 = unlimited.
	MemBudget int64

	// WebhookBase is the advertised base URL of the daemon's incoming-webhook
	// server (e.g. "http://127.0.0.1:8899"). "" = server not running, so the
	// webhook tools report unavailable instead of handing out dead URLs.
	WebhookBase string

	// IdleTimeout pages out a session that has produced no output for this long
	// (it's genuinely idle, not mid-work). Its state survives via --resume, so it
	// wakes on next use. 0 = never page out on idleness alone (budget only).
	IdleTimeout time.Duration

	runner   runner.Runner
	smu      sync.Mutex
	sessions map[addr.Address]runner.Session
	lastUsed map[addr.Address]int64 // logical-clock LRU stamp per bubble
	clock    int64

	// TypingWindow is how long after a keystroke the operator counts as "actively
	// typing" in the focused bubble (messages held so they don't submit a
	// half-typed line). 0 => 10s default.
	TypingWindow time.Duration

	focusMu     sync.Mutex
	focused     addr.Address // the bubble the operator is dived into
	heldFlushed bool         // whether the current idle backlog has already been flushed
	lastKey     atomic.Int64 // unixnano of the operator's last keystroke in the focused bubble

	notifyMu  sync.Mutex
	notified  map[addr.Address]int       // per bubble: >0 once its current backlog has been announced (nudge dedup)
	lastNudge map[addr.Address]time.Time // when we last wrote a notice to it (for stale-notice recovery)
}

func (k *Kernel) clearNudge(a addr.Address) {
	k.notifyMu.Lock()
	k.notified[a] = 0
	// The recovery clock is cleared too: the bubble has just drained its
	// inbox, so the next arrival is a fresh backlog that deserves a prompt
	// announcement rather than waiting out the previous notice's staleness.
	delete(k.lastNudge, a)
	k.notifyMu.Unlock()
}

// SetFocus marks the bubble the operator is dived into. Entry starts IDLE (not
// typing) — diving in is not typing — so a message delivers/flush promptly while
// you're just viewing; only real keystrokes (NoteKeystroke) mark active typing.
func (k *Kernel) SetFocus(a addr.Address) {
	k.lastKey.Store(0) // reset any carried-over typing state from another bubble
	k.focusMu.Lock()
	k.focused, k.heldFlushed = a, false
	k.focusMu.Unlock()
}

// NoteKeystroke records an operator keystroke in the focused bubble (called per
// key by the dive-in loop) so we can tell active typing from idle viewing.
func (k *Kernel) NoteKeystroke() { k.lastKey.Store(time.Now().UnixNano()) }

// typingActive reports whether the operator has typed within TypingWindow.
func (k *Kernel) typingActive() bool {
	w := k.TypingWindow
	if w <= 0 {
		w = 10 * time.Second
	}
	last := k.lastKey.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < w
}

// UnsetFocus clears focus and, if the just-left bubble has unread mail, nudges it
// once so the agent picks the backlog up.
func (k *Kernel) UnsetFocus(a addr.Address) {
	k.focusMu.Lock()
	if k.focused == a {
		k.focused = ""
	}
	k.focusMu.Unlock()
	if n := k.Store.UnreadCount(a); n > 0 && k.announceOnce(a, n) {
		if s := k.session(a); s != nil && s.Alive() {
			_, _ = s.Write([]byte(notify.RenderDrain(n)))
		}
	}
}

// FlushHeldIfIdle delivers the focused bubble's backlog once the operator stops
// typing — so a message that arrived mid-keystroke shows up as soon as you pause,
// not only when you leave. Called on a short ticker. It nudges at most once per
// typing pause (heldFlushed), and does nothing while typing is active.
func (k *Kernel) FlushHeldIfIdle() {
	if k.typingActive() {
		k.focusMu.Lock()
		k.heldFlushed = false // typing resumed; a later pause may flush again
		k.focusMu.Unlock()
		return
	}
	k.focusMu.Lock()
	foc := k.focused
	if foc == "" || k.heldFlushed {
		k.focusMu.Unlock()
		return
	}
	k.heldFlushed = true
	k.focusMu.Unlock()
	if n := k.Store.UnreadCount(foc); n > 0 && k.announceOnce(foc, n) {
		if s := k.session(foc); s != nil && s.Alive() {
			_, _ = s.Write([]byte(notify.RenderDrain(n)))
		}
	}
}

// operatorPresent reports whether the operator has typed anything recently —
// the signal that a dive/focus is live rather than a stale focus left behind by
// a detached terminal. The window is generous (2m) so an operator who is
// reading rather than typing still counts as present.
func (k *Kernel) operatorPresent() bool {
	last := k.lastKey.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < 2*time.Minute
}

// isFocused reports whether a is the currently dived-into bubble.
func (k *Kernel) isFocused(a addr.Address) bool {
	k.focusMu.Lock()
	defer k.focusMu.Unlock()
	return a != "" && a == k.focused
}

// New builds a Kernel over the given runner, with root seeded.
func New(r runner.Runner) *Kernel {
	k := &Kernel{
		Bus:           bus.New(),
		Caps:          caps.New(),
		Reg:           registry.New(),
		Store:         inbox.New(),
		Groups:        groups.New(),
		Sched:         sched.New(),
		Tasks:         tasks.New(),
		RelaunchProbe: 800 * time.Millisecond,
		runner:        r,
		sessions:      map[addr.Address]runner.Session{},
		lastUsed:      map[addr.Address]int64{},
		notified:      map[addr.Address]int{},
		lastNudge:     map[addr.Address]time.Time{},
		Cost:          costmeter.New(),
	}
	// The ceiling is constructed with the package defaults and has no bypass:
	// it is the last line of defense against the 632fe95 flood, so nothing
	// above it (rule, capability, config) can raise or disable it.
	k.Notify = notify.NewPolicy(k.muteRules, notify.NewCeiling(notify.DefaultCeilingPerMinute, notify.DefaultCeilingBurst))
	return k
}

// touch stamps a as most-recently-used (for LRU eviction).
func (k *Kernel) touch(a addr.Address) {
	k.smu.Lock()
	k.clock++
	k.lastUsed[a] = k.clock
	k.smu.Unlock()
}

// IsHot reports whether a has a live (resident) session.
func (k *Kernel) IsHot(a addr.Address) bool {
	s := k.session(a)
	return s != nil && s.Alive()
}

// EnforceBudget pages out the least-recently-used worker sessions until the sum
// of live sessions' ACTUAL resident memory fits within MemBudget (root is always
// resident and never counts). Paging out = kill the process; the registry entry,
// inbox, and session id persist, so the bubble resumes on next use. Called after
// each launch and periodically by the app, so a session that grows over time is
// caught. Measuring is done outside the lock.
func (k *Kernel) EnforceBudget() {
	if k.MemBudget <= 0 {
		return
	}
	k.smu.Lock()
	type ent struct {
		a    addr.Address
		s    runner.Session
		used int64
	}
	var live []ent
	for a, s := range k.sessions {
		if a == addr.Root || s == nil || !s.Alive() || k.isAlwaysOn(a) {
			continue // always-on receivers are never paged out for budget
		}
		live = append(live, ent{a, s, k.lastUsed[a]})
	}
	k.smu.Unlock()

	var total uint64
	mem := make(map[addr.Address]uint64, len(live))
	for _, e := range live {
		m := e.s.MemBytes()
		mem[e.a] = m
		total += m
	}
	if int64(total) <= k.MemBudget {
		return
	}
	sort.Slice(live, func(i, j int) bool { return live[i].used < live[j].used }) // coldest first
	var victims []addr.Address
	for _, e := range live {
		if int64(total) <= k.MemBudget {
			break
		}
		victims = append(victims, e.a)
		total -= mem[e.a]
	}
	k.smu.Lock()
	for _, v := range victims {
		delete(k.sessions, v)
	}
	k.smu.Unlock()
	for _, v := range victims {
		_ = k.runner.Kill(v) // page out
	}
}

// RelaunchSession bounces a HOT bubble's session so config that is baked in at
// launch picks up a change made while it was running — specifically the model
// (--model) and whether the spawn/edit/delete/introduce/broadcast tools are
// offered (BUBBLE_SPAWNABLE is written from CanSpawn when the mcp-config is
// generated, so a live session keeps its old tool list until relaunched). The
// conversation resumes via --resume; an in-progress turn is lost. No-op for root
// or a cold bubble — its next launch already uses current config.
func (k *Kernel) RelaunchSession(a addr.Address) {
	if a.IsRoot() || !k.IsHot(a) {
		return
	}
	k.smu.Lock()
	delete(k.sessions, a)
	delete(k.lastUsed, a)
	k.smu.Unlock()
	_ = k.runner.Kill(a)
	k.EnsureAlive(a) // relaunch with current config (resumes the conversation)
}

// SetEnabled parks or un-parks a bubble. Disabling hides it from every bubble's
// contacts, stops it immediately (kills its session), and blocks any relaunch —
// so it can't be woken by a dive, message, schedule, or webhook until re-enabled.
// Root can't be disabled. Its inbox and conversation are preserved; on re-enable
// it wakes and resumes normally.
func (k *Kernel) SetEnabled(a addr.Address, enabled bool) {
	if a.IsRoot() {
		return
	}
	k.Reg.SetDisabled(a, !enabled)
	if !enabled { // stop it now
		k.smu.Lock()
		s := k.sessions[a]
		delete(k.sessions, a)
		delete(k.lastUsed, a)
		k.smu.Unlock()
		if s != nil {
			_ = s.Close()
		}
		_ = k.runner.Kill(a)
	}
}

// EvictIdle pages out live worker sessions that have produced no output for
// longer than IdleTimeout — genuinely idle sessions (sitting at a prompt), as
// opposed to ones actively working (which stream output). Their conversation
// survives via --resume, so they wake on next use. Root is never touched. This
// keeps the resident set to sessions that are actually doing something, instead
// of holding every session hot until the memory budget forces eviction.
// isAlwaysOn reports whether a is an always-on receiver.
func (k *Kernel) isAlwaysOn(a addr.Address) bool {
	if b, ok := k.Reg.Get(a); ok {
		return b.AlwaysOn
	}
	return false
}

// KeepAlive launches any always-on receiver that isn't currently running, so a
// critical receiver always has a live session ready for instant delivery.
// Called at boot and on a periodic sweep.
func (k *Kernel) KeepAlive() {
	for _, a := range k.Reg.AlwaysOnAddrs() {
		if s := k.session(a); s == nil || !s.Alive() {
			k.EnsureAlive(a)
		}
	}
}

func (k *Kernel) EvictIdle() {
	if k.IdleTimeout <= 0 {
		return
	}
	cutoff := k.clockNow().Add(-k.IdleTimeout)
	k.smu.Lock()
	var idle []addr.Address
	for a, s := range k.sessions {
		if a == addr.Root || s == nil || !s.Alive() {
			continue
		}
		if k.isAlwaysOn(a) {
			continue // always-on receivers stay hot for instant delivery
		}
		if s.LastActivity().Before(cutoff) {
			idle = append(idle, a)
		}
	}
	for _, a := range idle {
		delete(k.sessions, a)
	}
	k.smu.Unlock()
	for _, a := range idle {
		_ = k.runner.Kill(a) // page out; registry + session id persist -> resumes on use
	}
}

// Usage is a per-session resource snapshot for the dashboard.
type Usage struct {
	Addr addr.Address
	Name string
	Mem  uint64        // resident bytes
	CPU  time.Duration // cumulative CPU consumed
}

// SampleUsage returns a resource snapshot of every live worker session (root
// excluded). CPU is cumulative; the caller diffs successive samples over wall
// time to get a percentage. Measuring is done outside the session lock.
func (k *Kernel) SampleUsage() []Usage {
	k.smu.Lock()
	type ent struct {
		a addr.Address
		s runner.Session
	}
	var live []ent
	for a, s := range k.sessions {
		if a == addr.Root || s == nil || !s.Alive() {
			continue
		}
		live = append(live, ent{a, s})
	}
	k.smu.Unlock()

	out := make([]Usage, 0, len(live))
	for _, e := range live {
		name := e.a.String()
		if b, ok := k.Reg.Get(e.a); ok {
			name = b.Label()
		}
		out = append(out, Usage{Addr: e.a, Name: name, Mem: e.s.MemBytes(), CPU: e.s.CPUTime()})
	}
	return out
}

// clockNow returns the wall clock (indirected so tests could stub it if needed).
func (k *Kernel) clockNow() time.Time { return time.Now() }

func (k *Kernel) setSession(a addr.Address, s runner.Session) {
	k.smu.Lock()
	k.sessions[a] = s
	k.smu.Unlock()
}

func (k *Kernel) session(a addr.Address) runner.Session {
	k.smu.Lock()
	defer k.smu.Unlock()
	return k.sessions[a]
}

// Send files a message in the recipient's inbox. The recipient is then notified
// without interruption: a short "you have mail" line is queued into its session
// (picked up on its next turn), prompting it to call inbox(). Messages to root
// blink the dashboard instead.
func (k *Kernel) Send(from, to addr.Address, subject, body string, replyTo int, urgent bool) (int, error) {
	if !k.Caps.CanSend(from, to) {
		return 0, ErrNotContact
	}
	if !from.IsRoot() && !to.IsRoot() {
		if b, ok := k.Reg.Get(to); ok && b.Disabled {
			return 0, ErrDisabled
		}
	}
	// Enforced route, cheap half: a worker with an open task cannot pass an
	// unverified "done" off to its assigner — the subject is annotated so only
	// the kernel's own "✅ task verified" notice reads as completion. (One map
	// scan; send stays fast and is never blocked.)
	if tid := k.Tasks.OpenBetween(from, to); tid != "" {
		subject = "[task " + tid + " open — unverified] " + subject
	}
	return k.fileAndNotify(from, to, subject, body, replyTo, urgent), nil
}

// fileAndNotify does the actual delivery (files the message, wires the reply
// grant, and nudges the recipient) WITHOUT a permission check — callers that are
// already authorized (Send after CanSend, BroadcastBy over its own subtree) use
// it directly.
func (k *Kernel) fileAndNotify(from, to addr.Address, subject, body string, replyTo int, urgent bool) int {
	fromName := ""
	if b, ok := k.Reg.Get(from); ok {
		fromName = b.Label()
	}
	return k.deliverMessage(from, fromName, to, subject, body, replyTo, urgent, true)
}

// deliverMessage is the shared delivery core. replyGrant is false for
// programmatic senders (webhooks) that have no address a bubble could reply to.
func (k *Kernel) deliverMessage(from addr.Address, fromName string, to addr.Address, subject, body string, replyTo int, urgent, replyGrant bool) int {
	// An always-on receiver treats EVERY message as urgent: deliver to its (kept-
	// hot) session immediately, never leave it pooled for the slow drain. This is
	// what removes the route-urgency dependency for critical receivers.
	if k.isAlwaysOn(to) {
		urgent = true
	}
	id := k.Store.Append(inbox.Message{From: from, FromName: fromName, To: to, Subject: subject, Body: body, ReplyTo: replyTo})
	if replyGrant {
		k.Caps.AddContact(to, from) // reply grant: the recipient can always reply to whoever messaged it
	}
	if to == addr.Root {
		_ = k.Bus.Send(bus.Message{From: from, To: to, Subject: subject, Body: body}) // blink the dashboard (root is the human; always notified)
	}
	// Pre-flight, BEFORE any policy state is spent. Both arms below end in "no
	// write happens", and Decide MUTATES: it spends an INV-1 token and, on the
	// non-urgent path, opens a coalescing window that policy.go grants only to
	// a message that is actually written. Deciding first and bailing after
	// would let followers buffer against a window this kernel never honoured,
	// and would discard a rendered Inline body with no counter behind it.
	//
	// While the operator is ACTIVELY TYPING in the recipient, hold the
	// notification: typing it in would submit their half-written line. It
	// stays in the inbox and is delivered as soon as they pause
	// (FlushHeldIfIdle) or leave (UnsetFocus). If they're just idle-viewing
	// the bubble, deliver normally so they see it.
	if k.isFocused(to) && k.typingActive() {
		return id
	}
	hot := k.IsHot(to)
	if !urgent && !hot {
		// A previously-run but paged-out bubble (it has a SessionID) stays
		// pooled for the periodic DrainInboxes, so we don't wake a whole
		// sleeping fleet on every message. Only a never-launched bubble boots
		// here: a freshly-spawned worker awaiting its first task must start
		// when a charter is delegated to it -- the "spawn a worker and message
		// it a charter" flow.
		if b, ok := k.Reg.Get(to); !ok || to.IsRoot() || b.SessionID != "" {
			return id
		}
	}

	// Notification policy runs HERE, before anything that could page the
	// recipient in. A muted message must not wake a cold bubble: the wake is a
	// full prompt-cache rewarm and is the dominant cost of noisy traffic, so
	// suppressing only the notice would leave that cost untouched. The message
	// is already filed above, so a Suppress loses nothing.
	//
	// The match key a mute rule is written against is the STABLE identifier:
	// the webhook source for programmatic mail, the sender's address
	// otherwise. The decorated persona label is display only -- matching on it
	// would break every rule the day a sender is renamed.
	source := from.String()
	if from == WebhookFrom && fromName != "" {
		source = fromName
	}
	display := from.String()
	if fromName != "" {
		display += " (" + fromName + ")"
	}
	d, prevAnnounced, notifiable := k.decide(to, id, source, display, subject, body, urgent, hot)
	if !notifiable {
		return id
	}

	wrote := false
	switch {
	case urgent:
		// NOTE: d.Wake is near-vacuous on this arm -- for an urgent message it
		// is true exactly when the bubble is cold, so `hot || d.Wake` is
		// almost always true. That is fine and it is NOT the safety gate. The
		// real wake veto is the `!notifiable` early return above: a muted
		// message returns there and never reaches EnsureAlive, which is the
		// entire cost saving of this phase. Do not "simplify" this condition
		// away on the grounds that it looks redundant -- keep the Suppress
		// return, and keep d.Wake here so a future non-urgent wake policy
		// (AlwaysOn, or a Rollup that must never wake) is honoured.
		if hot || d.Wake {
			wrote = k.writeNotice(to, k.EnsureAlive(to), d)
		}
	case hot:
		k.touch(to)
		wrote = k.writeNotice(to, k.session(to), d)
	default:
		// First launch: pre-flight above already established this is a
		// never-launched, non-root bubble.
		wrote = k.writeNotice(to, k.EnsureAlive(to), d)
	}
	if !wrote {
		// Unreachable after all (disabled bubble, failed launch): give back
		// the announcement claim so the backlog is not recorded as advertised
		// when no notice exists.
		k.unclaimAnnounced(to, d.Announce, prevAnnounced)
	}
	return id
}

// deliverWhenReady waits until a's session has produced output — its TUI is up
// and accepting input — before typing the notice in, so a message that boots a
// cold bubble isn't typed into a still-initializing claude (where it would sit
// unsubmitted until something else pressed Enter). Bounded by a timeout, after
// which it delivers anyway. Runs off the send path (in a goroutine).
func (k *Kernel) deliverWhenReady(a addr.Address, line []byte) {
	k.deliverWhenReadyThen(a, line, nil)
}

// deliverWhenReadyThen is deliverWhenReady with a completion hook that runs
// ONLY after the line has actually been written. Anything that treats the
// message as consumed belongs there and nowhere else: this function can return
// without writing at all (the session died while booting, or never became
// ready before the deadline), and a message marked consumed by a write that
// never happened is content nobody will ever see.
func (k *Kernel) deliverWhenReadyThen(a addr.Address, line []byte, onWritten func()) {
	write := func(s runner.Session) {
		if _, err := s.Write(line); err == nil && onWritten != nil {
			onWritten()
		}
	}
	deadline := time.Now().Add(35 * time.Second) // covers a long resume + the summary-menu autopilot
	for time.Now().Before(deadline) {
		s := k.session(a)
		if s == nil || !s.Alive() {
			return // never written: the caller's consume hook must not run
		}
		if s.InputReady() {
			write(s)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s := k.session(a); s != nil && s.Alive() {
		write(s)
	}
}

// DrainInboxes is the periodic full sweep: it wakes cold bubbles that have unread
// mail and (re)nudges them, so no message goes unseen — including after a restart,
// where every bubble with pending mail gets re-notified.
func (k *Kernel) DrainInboxes() { k.RecoverUnread(false) }

// RecoverUnread nudges bubbles that have unread mail they haven't been told about
// (or whose earlier notice went stale). It's the safety net behind the fast send
// path: a lost/never-landed notice is recovered here, so an inbox can't grow
// silently. hotOnly=true does the cheap frequent pass (only bubbles already
// running — just a PTY write); hotOnly=false is the full drain, paging cold
// bubbles in with bounded concurrency. The focused bubble is skipped so the
// operator's typing isn't interrupted.
func (k *Kernel) RecoverUnread(hotOnly bool) {
	type job struct {
		a    addr.Address
		d    notify.Decision
		prev int
	}
	var jobs []job
	operatorAway := !k.operatorPresent()
	for _, b := range k.Reg.All() {
		if b.Addr.IsRoot() {
			continue
		}
		// Skip the focused bubble only while the operator is actually AROUND
		// (recent keystrokes). Focus is not unset when a terminal client
		// detaches mid-dive, so skipping unconditionally exempted exactly the
		// bubble the operator lives in from recovery — unread mail sat there for
		// hours until a manual reopen (3cfb0d9).
		if k.isFocused(b.Addr) && !operatorAway {
			continue
		}
		hot := k.IsHot(b.Addr)
		if hotOnly && !hot {
			continue
		}
		// One decision point with the send path: the notifiable count is read,
		// the policy consulted and the announcement claimed in a single critical
		// section shared with deliverMessage, so a send racing this sweep cannot
		// produce a second notice for the same backlog.
		if d, prev, ok := k.decideRecovery(b.Addr, hot); ok {
			jobs = append(jobs, job{b.Addr, d, prev})
		}
	}
	if len(jobs) == 0 {
		return
	}
	const workers = 4
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			var s runner.Session
			if hotOnly {
				s = k.session(j.a)
			} else if j.d.Wake {
				s = k.EnsureAlive(j.a) // page a cold bubble in so it has a terminal to notify
			}
			// writeNotice handles the still-booting case (deferred write) and
			// meters what it costs; no PTY write happens under notifyMu.
			if !k.writeNotice(j.a, s, j.d) {
				// Unreachable after all (disabled bubble, failed launch): give
				// back the claim so the backlog is not recorded as advertised
				// when no notice exists, and the next sweep retries it.
				k.unclaimAnnounced(j.a, j.d.Announce, j.prev)
			}
		}(j)
	}
	wg.Wait()
}

// EnsureAlive returns a live session for a, relaunching it if its process has
// died. It first tries to resume the bubble's prior conversation; if that
// session id no longer exists (the resumed process exits at once), it starts a
// fresh session seeded with the persona. Root is never auto-launched here (it is
// managed via StartRoot / dive-in). Returns nil if a has no launchable session.
func (k *Kernel) EnsureAlive(a addr.Address) runner.Session {
	cur := k.session(a)
	if a.IsRoot() {
		return cur
	}
	if cur != nil && cur.Alive() {
		k.touch(a) // used -> most-recently-used
		return cur
	}
	b, ok := k.Reg.Get(a)
	if !ok {
		return cur // unregistered address: nothing to relaunch
	}
	if b.Disabled {
		return nil // parked: refuse to launch until re-enabled
	}
	if cur != nil {
		_ = cur.Close()
	}
	// If the session switched conversations (/resume) while it was running, resume
	// the one it's actually on now, not the id we launched with.
	if k.CurrentSessionID != nil {
		if cur := k.CurrentSessionID(a); cur != "" {
			b.SessionID = cur
		}
	}
	// Try to resume the existing conversation.
	if b.SessionID != "" {
		if sess, err := k.runner.Launch(a, b.Dir, runner.SpawnOpts{Persona: b.Label(), Model: b.Model, SessionID: b.SessionID, Resume: true}); err == nil {
			// A resume can fail two ways: the process exits (Alive false), or claude
			// has no record of the id and prints "No conversation found" (e.g. the
			// session was never persisted because the bubble stalled, or its working
			// dir changed). Either way, fall through to a fresh session — which keeps
			// the bubble's pending inbox messages (those live in our store).
			if k.resumeHealthy(sess) {
				k.setSession(a, sess)
				k.touch(a)
				k.EnforceBudget() // page in -> may page out a colder bubble
				return sess
			}
			_ = sess.Close()
		}
	}
	// Fresh session with a new id, seeded with the persona and its charter/goal.
	b.SessionID = newSessionID()
	sess, err := k.runner.Launch(a, b.Dir, runner.SpawnOpts{Persona: b.Label(), Goal: b.Goal, Model: b.Model, SessionID: b.SessionID, Resume: false})
	if err != nil {
		return nil
	}
	if k.RelaunchProbe > 0 {
		time.Sleep(k.RelaunchProbe) // a launch that dies at once (bad dir/args) shouldn't be handed back as a live PTY
		if !sess.Alive() {
			_ = sess.Close()
			return nil
		}
	}
	k.setSession(a, sess)
	k.touch(a)
	k.EnforceBudget()
	return sess
}

// Status reports the delivery state of messages from sent: delivered / read /
// replied, each labeled with the recipient's address and role.
func (k *Kernel) Status(from addr.Address) []string {
	var out []string
	for _, m := range k.Store.Sent(from) {
		state := "delivered"
		if m.Read {
			state = "read, no reply"
		}
		if m.Replied {
			state = "replied"
		}
		to := m.To.String()
		if b, ok := k.Reg.Get(m.To); ok && b.Label() != "" {
			to += " (" + b.Label() + ")"
		}
		out = append(out, fmt.Sprintf("[%d] to %s — %q: %s", m.ID, to, m.Subject, state))
	}
	return out
}

// Inbox returns owner's unread messages (marking them read), each labeled with
// the sender's address and role.
func (k *Kernel) Inbox(owner addr.Address) []string {
	k.clearNudge(owner) // read -> a future message re-announces
	var out []string
	for _, m := range k.Store.Take(owner) {
		from := m.From.String()
		if m.FromName != "" {
			from += " (" + m.FromName + ")"
		}
		out = append(out, fmt.Sprintf("[%d] from %s — %s: %s", m.ID, from, m.Subject, m.Body))
	}
	return out
}

// Contacts returns who owner may message, filtered to bubbles that still exist
// (root always qualifies). This is belt-and-suspenders against a stale capability
// edge ever surfacing a deleted bubble as a ghost contact.
func (k *Kernel) Contacts(owner addr.Address) []addr.Address {
	var out []addr.Address
	for _, c := range k.Caps.Contacts(owner) {
		if c.IsRoot() {
			out = append(out, c)
			continue
		}
		if b, ok := k.Reg.Get(c); ok && !b.Disabled { // hide disabled bubbles from contact lists
			out = append(out, c)
		}
	}
	return out
}

// Forget drops one of owner's contacts so it can no longer send to it (root is
// never removable). Used by the forget() tool for contact-list hygiene.
func (k *Kernel) Forget(owner, contact addr.Address) error {
	if contact.IsRoot() {
		return ErrNotAllowed
	}
	k.Caps.RemoveContact(owner, contact)
	return nil
}

// SyncSessionIDs refreshes each bubble's stored SessionID from its live session
// hook, so a conversation switched via /resume in a still-running session is
// captured before the fleet is persisted. No-op without CurrentSessionID.
func (k *Kernel) SyncSessionIDs() {
	if k.CurrentSessionID == nil {
		return
	}
	for _, b := range k.Reg.All() {
		if cur := k.CurrentSessionID(b.Addr); cur != "" {
			b.SessionID = cur
		}
	}
}

// Compact asks a running bubble to compact its OWN conversation now — typing the
// `/compact` slash command (with an optional focus) into its session, queued for
// after its current turn. A bubble calls this at a natural checkpoint (task done,
// key context written down) so its context is reclaimed deliberately instead of
// auto-compacting only once it's near the limit. No-op if the bubble is cold.
func (k *Kernel) Compact(a addr.Address, focus string) error {
	s := k.session(a)
	if s == nil || !s.Alive() {
		return fmt.Errorf("kernel: %s is not running", a)
	}
	cmd := "/compact"
	// The focus comes from a calling bubble, and this is typed into another
	// session: unsanitised it is a keystroke-injection path.
	if focus = notify.Sanitize(strings.TrimSpace(focus)); focus != "" {
		cmd += " " + focus
	}
	_, err := s.Write([]byte(cmd)) // Write appends Enter, submitting the command
	return err
}

// WebhookFrom is the pseudo-address programmatic (webhook) messages arrive from.
// It is not a real bubble: no contacts point at it and it can't be replied to.
const WebhookFrom = addr.Address("webhook")

// WebhookURL returns a bubble's incoming-webhook URL, minting its secret token
// on first use (persisted, so the URL is stable across restarts).
func (k *Kernel) WebhookURL(a addr.Address) (string, error) {
	if k.WebhookBase == "" {
		return "", errors.New("kernel: webhook server not running (port busy or disabled)")
	}
	b, ok := k.Reg.Get(a)
	if !ok {
		return "", fmt.Errorf("kernel: no bubble at %s", a)
	}
	if b.WebhookToken == "" {
		k.Reg.SetWebhookToken(a, newWebhookToken())
		b, _ = k.Reg.Get(a)
	}
	return k.WebhookBase + "/w/" + b.WebhookToken, nil
}

// RotateWebhook revokes a bubble's current webhook URL and issues a fresh one.
func (k *Kernel) RotateWebhook(a addr.Address) (string, error) {
	if _, ok := k.Reg.Get(a); !ok {
		return "", fmt.Errorf("kernel: no bubble at %s", a)
	}
	k.Reg.SetWebhookToken(a, newWebhookToken())
	return k.WebhookURL(a)
}

// WebhookURLBy is WebhookURL with the fleet's ownership rule: `by` may fetch the
// webhook URL of itself or any bubble in its own subtree (root: anyone) — the
// same authority as edit/delete/schedule.
func (k *Kernel) WebhookURLBy(by, target addr.Address) (string, error) {
	if !k.canManage(by, target) {
		return "", ErrNotAllowed
	}
	return k.WebhookURL(target)
}

// RotateWebhookBy applies the same ownership rule to rotation.
func (k *Kernel) RotateWebhookBy(by, target addr.Address) (string, error) {
	if !k.canManage(by, target) {
		return "", ErrNotAllowed
	}
	return k.RotateWebhook(target)
}

// ResolveWebhookToken maps an incoming /w/<token> hit to its bubble.
func (k *Kernel) ResolveWebhookToken(tok string) (addr.Address, bool) {
	if b, ok := k.Reg.ByWebhookToken(tok); ok {
		return b.Addr, true
	}
	return "", false
}

// ControlWebhookURL returns the CONTROL webhook URL for a spawn-granted bubble —
// a distinct /c/<token> endpoint that runs fleet actions (spawn/delete/list) AS
// that bubble, governed by its own spawn capability. Denied for a bubble that
// cannot spawn, so only management-capable bubbles ever get a control surface.
func (k *Kernel) ControlWebhookURL(by addr.Address) (string, error) {
	if k.WebhookBase == "" {
		return "", errors.New("kernel: webhook server not running (port busy or disabled)")
	}
	if !k.Caps.CanSpawn(by) {
		return "", ErrNotAllowed
	}
	b, ok := k.Reg.Get(by)
	if !ok {
		return "", fmt.Errorf("kernel: no bubble at %s", by)
	}
	if b.ControlToken == "" {
		k.Reg.SetControlToken(by, newWebhookToken())
		b, _ = k.Reg.Get(by)
	}
	return k.WebhookBase + "/c/" + b.ControlToken, nil
}

// RotateControlWebhook revokes a bubble's control URL and issues a fresh one.
func (k *Kernel) RotateControlWebhook(by addr.Address) (string, error) {
	if !k.Caps.CanSpawn(by) {
		return "", ErrNotAllowed
	}
	if _, ok := k.Reg.Get(by); !ok {
		return "", fmt.Errorf("kernel: no bubble at %s", by)
	}
	k.Reg.SetControlToken(by, newWebhookToken())
	return k.ControlWebhookURL(by)
}

// ResolveControlToken maps an incoming /c/<token> hit to the bubble that acts as
// the caller — actions execute with this bubble's authority.
func (k *Kernel) ResolveControlToken(tok string) (addr.Address, bool) {
	if b, ok := k.Reg.ByControlToken(tok); ok {
		return b.Addr, true
	}
	return "", false
}

// WebhookDeliver files a PROGRAMMATIC message (from a script/cron/external
// service via the bubble's webhook URL) and wakes the target like any other
// delivery. source labels the sender for display ("webhook (ci)"); there is no
// reply grant — the recipient can tell it was a machine, and can't reply.
func (k *Kernel) WebhookDeliver(target addr.Address, source, subject, body string, urgent bool) (int, error) {
	if _, ok := k.Reg.Get(target); !ok {
		return 0, fmt.Errorf("kernel: no bubble at %s", target)
	}
	return k.deliverMessage(WebhookFrom, source, target, subject, body, 0, urgent, false), nil
}

// newWebhookToken returns an unguessable URL-safe secret.
func newWebhookToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

// canManage reports whether `by` may schedule/manage wakes for target: root
// anywhere, otherwise only itself or a bubble in its own subtree.
func (k *Kernel) canManage(by, target addr.Address) bool {
	return by == addr.Root || by == target || by.IsAncestorOf(target)
}

// ScheduleBy registers a durable wake: the daemon will deliver {subject, body}
// to target on the trigger, waking it if cold. `by` may schedule itself or a
// bubble it owns (root: anyone). Returns the schedule id.
func (k *Kernel) ScheduleBy(by, target addr.Address, subject, body string, trig sched.Trigger, urgent bool) (string, error) {
	if !k.canManage(by, target) {
		return "", ErrNotAllowed
	}
	if _, ok := k.Reg.Get(target); !ok {
		return "", fmt.Errorf("kernel: no bubble at %s", target)
	}
	sc := k.Sched.Add(k.clockNow(), by, target, subject, body, trig, urgent)
	return sc.ID, nil
}

// UnscheduleBy removes a schedule the caller is allowed to manage (it created it,
// or it owns/targets the bubble; root: any).
func (k *Kernel) UnscheduleBy(by addr.Address, id string) error {
	sc, ok := k.Sched.Get(id)
	if !ok {
		return fmt.Errorf("kernel: no schedule %s", id)
	}
	if by != sc.From && !k.canManage(by, sc.Target) {
		return ErrNotAllowed
	}
	k.Sched.Remove(id)
	return nil
}

// SchedulesFor returns the schedules the caller may see: ones it created or that
// target its subtree/itself (root sees all), each rendered for display.
func (k *Kernel) SchedulesFor(by addr.Address) []string {
	var out []string
	for _, sc := range k.Sched.All() {
		if by != sc.From && !k.canManage(by, sc.Target) {
			continue
		}
		out = append(out, fmt.Sprintf("[%s] -> %s %q (%s) next %s", sc.ID, sc.Target, sc.Subject, sc.Trigger.String(), sc.NextFire.Format("Jan 2 15:04")))
	}
	sort.Strings(out)
	return out
}

// FireDue delivers every schedule that has come due — waking each target via the
// normal delivery path (cold bubbles page in). Called on a short daemon tick.
func (k *Kernel) FireDue() {
	for _, sc := range k.Sched.Due(k.clockNow()) {
		if _, ok := k.Reg.Get(sc.Target); !ok {
			k.Sched.Remove(sc.ID) // target vanished — drop it
			continue
		}
		k.fileAndNotify(sc.From, sc.Target, sc.Subject, sc.Body, 0, sc.Urgent)
	}
}

// Introduce makes a and b mutual contacts. Root only.
func (k *Kernel) Introduce(by, a, b addr.Address) error {
	if by != addr.Root {
		return ErrNotAllowed
	}
	k.Caps.Introduce(a, b)
	return nil
}

// IntroduceBy lets a bubble introduce two bubbles it OWNS (both in its subtree)
// to each other, so its sub-workers can hand off directly instead of relaying
// through it. Root may introduce anyone; a non-root bubble is confined to its own
// descendants (the human stays the gate for cross-tree links). Both targets must
// exist.
func (k *Kernel) IntroduceBy(by, a, b addr.Address) error {
	if a == b {
		return ErrNotAllowed
	}
	if _, ok := k.Reg.Get(a); !ok {
		return fmt.Errorf("kernel: no bubble at %s", a)
	}
	if _, ok := k.Reg.Get(b); !ok {
		return fmt.Errorf("kernel: no bubble at %s", b)
	}
	if by != addr.Root && (!by.IsAncestorOf(a) || !by.IsAncestorOf(b)) {
		return ErrNotAllowed
	}
	k.Caps.Introduce(a, b)
	return nil
}

// BroadcastBy files a message to every descendant of by (its whole subtree) in
// one call — for fleet-wide notices to the workers you own. Authorized by
// ownership: no per-recipient contact check (the subtree is yours). Each
// recipient can reply to you. Returns how many bubbles it reached.
func (k *Kernel) BroadcastBy(by addr.Address, subject, body string, urgent bool) int {
	var targets []addr.Address
	for _, b := range k.Reg.All() {
		if by.IsAncestorOf(b.Addr) {
			targets = append(targets, b.Addr)
		}
	}
	for _, t := range targets {
		k.fileAndNotify(by, t, subject, body, 0, urgent)
	}
	return len(targets)
}

// Spawn creates a child bubble under by, launches its session, and wires
// delivery so messages to it are injected into the session. Fresh bubbles
// know only root.
func (k *Kernel) Spawn(by addr.Address, persona, dir string, opts runner.SpawnOpts) (addr.Address, error) {
	return k.SpawnUnder(by, by, persona, dir, opts)
}

// SpawnUnder is like Spawn but places the child under an explicit parent in the
// tree. `by` is the authority (its spawn capability is checked/consumed); parent
// is where the child is attached. The human dashboard spawns with by=root and
// parent=the selected bubble, so root can create a child anywhere.
func (k *Kernel) SpawnUnder(by, parent addr.Address, persona, dir string, opts runner.SpawnOpts) (addr.Address, error) {
	if !k.Caps.CanSpawn(by) {
		return "", ErrNotAllowed
	}
	if err := k.Caps.ConsumeSpawn(by); err != nil {
		return "", err
	}
	b := k.Reg.Add(parent, persona, dir)
	b.Name = opts.Name
	b.Model = opts.Model
	b.Goal = opts.Goal
	// Lazy launch: NO session id and NO process yet. The bubble is a cold record
	// (0 RAM) until first used — a dive, a message, or a loop trigger pages it in
	// via EnsureAlive. So you can spawn hundreds and only the touched ones run.
	k.Caps.AddContact(b.Addr, addr.Root)  // every bubble can reach root
	k.Caps.AddContact(b.Addr, parent)     // the child can reach its spawner (report progress, ask questions)
	k.Caps.AddContact(parent, b.Addr)     // the parent can reach its child

	// Spawn-ability grant. Root grants explicitly (opts.GrantSpawn => depth 1: the
	// child may spawn, but its own children may not). A non-root spawner can only
	// pass down depth-1, so a depth-1 bubble's children get nothing — an AI can
	// never hand its spawn grant to its children.
	if by == addr.Root {
		if opts.GrantSpawn {
			k.Caps.GrantSpawnDepth(b.Addr, 1)
		}
	} else if d := k.Caps.SpawnDepth(by); d > 1 {
		k.Caps.GrantSpawnDepth(b.Addr, d-1)
	}
	k.SeedBrain(b.Addr) // guaranteed private brain skeleton, keyed by address (workspaces may be shared)
	return b.Addr, nil
}

// resumeLost reports whether a --resume launch failed because claude has no
// record of the session id (it prints "No conversation found ..."). Detected
// from the session's recent output so we can fall back to a fresh session
// instead of leaving the bubble stuck on the error.
func resumeLost(s runner.Session) bool {
	return strings.Contains(s.RecentOutput(), "No conversation found")
}

// resumeProbeWindow bounds how long resumeHealthy waits for a doomed --resume to
// reveal itself. A doomed resume prints "No conversation found" and exits ~1.4s
// after launch (a LOCAL session-file check, so it's stable — not affected by the
// network or a compression proxy). The old single 800ms sleep raced ahead of
// that error and handed back the dying session; we poll the window instead and
// bail the instant the resume fails. A healthy resume waits out the window (a
// background wake path, not keystroke latency) — correctness over ~1.7s.
const resumeProbeWindow = 2500 * time.Millisecond

// resumeHealthy reports whether a freshly-resumed session came up on the real
// conversation. It polls (rather than sleeping once) so it heals regardless of
// how long claude takes to print "No conversation found" — the bug that left
// bubbles stuck on that error. RelaunchProbe <= 0 (tests) skips the wait.
func (k *Kernel) resumeHealthy(sess runner.Session) bool {
	if k.RelaunchProbe <= 0 {
		return sess.Alive() && !resumeLost(sess)
	}
	deadline := time.Now().Add(resumeProbeWindow)
	for time.Now().Before(deadline) {
		if resumeLost(sess) || !sess.Alive() {
			return false // doomed resume revealed itself — heal to a fresh session
		}
		time.Sleep(150 * time.Millisecond)
	}
	return sess.Alive() && !resumeLost(sess)
}

// newSessionID returns a random UUIDv4 string for tagging a claude session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CreateGroup records a named grouping. If introduceAll, every member becomes a
// mutual contact of every other (otherwise grouping shares no contacts).
func (k *Kernel) CreateGroup(name string, members []addr.Address, introduceAll bool) {
	k.Groups.Create(name, members)
	if introduceAll {
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				k.Caps.Introduce(members[i], members[j])
			}
		}
	}
}

// AttachGroupSession spawns a coordinator bubble for the group (a normal
// root-child bubble) and gives it every member as a contact, so the group
// session can message all members. Returns its address.
func (k *Kernel) AttachGroupSession(name, dir string, opts runner.SpawnOpts) (addr.Address, error) {
	g, ok := k.Groups.Get(name)
	if !ok {
		return "", ErrNotAllowed
	}
	a, err := k.SpawnUnder(addr.Root, addr.Root, "#"+name, dir, opts)
	if err != nil {
		return "", err
	}
	for _, m := range g.Members {
		k.Caps.AddContact(a, m) // the group session can reach each member
	}
	k.Groups.SetSession(name, a)
	return a, nil
}

// DeleteBubble removes a bubble and its entire subtree: each session is killed
// and dropped from the registry, the sessions map, and any group membership.
// Root cannot be deleted. Returns the addresses removed (so callers can clear
// number-slots pointing at them).
func (k *Kernel) DeleteBubble(a addr.Address) []addr.Address {
	if a.IsRoot() {
		return nil
	}
	var victims []addr.Address
	var collect func(x addr.Address)
	collect = func(x addr.Address) {
		for _, c := range k.Reg.Children(x) {
			collect(c.Addr)
		}
		victims = append(victims, x)
	}
	collect(a)
	for _, v := range victims {
		_ = k.runner.Kill(v)
		k.Reg.Remove(v)
		k.smu.Lock()
		delete(k.sessions, v)
		delete(k.lastUsed, v)
		k.smu.Unlock()
		k.Groups.PurgeMember(v)
		k.Caps.Purge(v)        // drop it from EVERY bubble's contacts, so no ghost lingers
		k.Sched.PurgeBubble(v) // drop any wake schedules for/by it
		k.Tasks.PurgeParticipant(v) // cancel its open tasks / degrade tasks it verified
	}
	return victims
}

// DeleteBy is DeleteBubble with an authority check for agent callers: by may
// delete only bubbles in its OWN subtree (the ones it spawned, and theirs).
// Root passes unconditionally (the dashboard). Returns the removed addresses.
func (k *Kernel) DeleteBy(by, a addr.Address) ([]addr.Address, error) {
	if by != addr.Root && !by.IsAncestorOf(a) {
		return nil, ErrNotAllowed
	}
	if _, ok := k.Reg.Get(a); !ok {
		return nil, fmt.Errorf("kernel: no bubble at %s", a)
	}
	victims := k.DeleteBubble(a)
	if victims == nil { // root target
		return nil, ErrNotAllowed
	}
	return victims, nil
}

// EditBy updates a bubble's settings with the same subtree authority as
// DeleteBy: by may edit only its own descendants (root may edit any bubble).
// Empty fields are left unchanged. Name/model apply on the next page-in; a new
// description (charter) only matters if the bubble hasn't launched yet.
func (k *Kernel) EditBy(by, a addr.Address, name, model, description string) error {
	if by != addr.Root && !by.IsAncestorOf(a) {
		return ErrNotAllowed
	}
	b, ok := k.Reg.Get(a)
	if !ok {
		return fmt.Errorf("kernel: no bubble at %s", a)
	}
	oldModel := b.Model
	if name != "" {
		k.Reg.SetName(a, name)
	}
	if model != "" {
		k.Reg.SetModel(a, model)
	}
	if description != "" {
		k.Reg.SetGoal(a, description)
	}
	if model != "" && model != oldModel {
		k.RelaunchSession(a) // model is a launch flag — bounce so the change takes effect now
	}
	return nil
}

// DeleteGroup removes a group. If deleteMembers, each member bubble (and its
// subtree) is deleted too; otherwise only the grouping is removed and members
// live on. A coordinator session is always killed. Contacts are left intact.
func (k *Kernel) DeleteGroup(name string, deleteMembers bool) {
	g, ok := k.Groups.Get(name)
	if ok && deleteMembers {
		for _, m := range g.Members {
			k.DeleteBubble(m)
		}
	}
	if ok && g.Session != "" {
		_ = k.runner.Kill(g.Session)
		k.Reg.Remove(g.Session)
		k.smu.Lock()
		delete(k.sessions, g.Session)
		k.smu.Unlock()
	}
	k.Groups.Delete(name)
}

// StartRoot launches root's own claude session in dir if it isn't already
// running, so the operator can dive into a top-level agent. Idempotent.
func (k *Kernel) StartRoot(dir string) error {
	if k.session(addr.Root) != nil {
		return nil
	}
	b, ok := k.Reg.Get(addr.Root)
	if !ok {
		return nil
	}
	if b.Dir == "" {
		b.Dir = dir
	}
	if k.CurrentSessionID != nil { // resume the conversation root is actually on now
		if cur := k.CurrentSessionID(addr.Root); cur != "" {
			b.SessionID = cur
		}
	}
	resume := b.SessionID != "" // set => restored, resume its conversation
	if b.SessionID == "" {
		b.SessionID = newSessionID()
	}
	sess, err := k.runner.Launch(addr.Root, b.Dir, runner.SpawnOpts{Persona: "root", SessionID: b.SessionID, Resume: resume})
	if err != nil {
		return err
	}
	k.setSession(addr.Root, sess)
	return nil
}
