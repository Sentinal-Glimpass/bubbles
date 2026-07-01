// Package kernel wires the bus, capabilities, registry, and a runner into the
// four fleet operations: Send, Contacts, Introduce, Spawn.
package kernel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/caps"
	"github.com/Sentinal-Glimpass/bubbles/internal/groups"
	"github.com/Sentinal-Glimpass/bubbles/internal/inbox"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// ErrNotContact is returned by Send when from may not message to.
var ErrNotContact = errors.New("kernel: recipient not in contacts")

// ErrNotAllowed is returned for root-only actions attempted by non-root.
var ErrNotAllowed = errors.New("kernel: action not permitted")

// Kernel is the fleet engine.
type Kernel struct {
	Bus    *bus.Bus
	Caps   *caps.Store
	Reg    *registry.Registry
	Store  *inbox.Store
	Groups *groups.Store

	// RelaunchProbe is how long EnsureAlive waits to see if a *resumed* session
	// survives before falling back to a fresh one (a doomed --resume exits fast).
	// Tests set it to 0; real use gives claude a moment to fail.
	RelaunchProbe time.Duration

	runner   runner.Runner
	smu      sync.Mutex
	sessions map[addr.Address]runner.Session
}

// New builds a Kernel over the given runner, with root seeded.
func New(r runner.Runner) *Kernel {
	return &Kernel{
		Bus:           bus.New(),
		Caps:          caps.New(),
		Reg:           registry.New(),
		Store:         inbox.New(),
		Groups:        groups.New(),
		RelaunchProbe: 800 * time.Millisecond,
		runner:        r,
		sessions:      map[addr.Address]runner.Session{},
	}
}

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
func (k *Kernel) Send(from, to addr.Address, subject, body string, replyTo int) (int, error) {
	if !k.Caps.CanSend(from, to) {
		return 0, ErrNotContact
	}
	fromName := ""
	if b, ok := k.Reg.Get(from); ok {
		fromName = b.Persona
	}
	id := k.Store.Append(inbox.Message{From: from, FromName: fromName, To: to, Subject: subject, Body: body, ReplyTo: replyTo})
	k.Caps.AddContact(to, from) // reply grant: the recipient can always reply to whoever messaged it
	if to == addr.Root {
		_ = k.Bus.Send(bus.Message{From: from, To: to, Subject: subject, Body: body}) // blink the dashboard
	}
	if s := k.EnsureAlive(to); s != nil { // heal a dead recipient, then inject the notice (incl. root, if running)
		unread := k.Store.UnreadCount(to)
		_, _ = s.Write([]byte(formatNotify(from, fromName, subject, unread)))
	}
	return id, nil
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
		return cur
	}
	b, ok := k.Reg.Get(a)
	if !ok {
		return cur // unregistered address: nothing to relaunch
	}
	if cur != nil {
		_ = cur.Close()
	}
	// Try to resume the existing conversation.
	if b.SessionID != "" {
		if sess, err := k.runner.Launch(a, b.Dir, runner.SpawnOpts{Persona: b.Persona, SessionID: b.SessionID, Resume: true}); err == nil {
			if k.RelaunchProbe > 0 {
				time.Sleep(k.RelaunchProbe) // give a doomed resume time to exit
			}
			if sess.Alive() {
				k.setSession(a, sess)
				return sess
			}
			_ = sess.Close() // resume failed (session id gone) -> fall through to fresh
		}
	}
	// Fresh session with a new id, seeded with the persona.
	b.SessionID = newSessionID()
	sess, err := k.runner.Launch(a, b.Dir, runner.SpawnOpts{Persona: b.Persona, SessionID: b.SessionID, Resume: false})
	if err != nil {
		return nil
	}
	k.setSession(a, sess)
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
		if b, ok := k.Reg.Get(m.To); ok && b.Persona != "" {
			to += " (" + b.Persona + ")"
		}
		out = append(out, fmt.Sprintf("[%d] to %s — %q: %s", m.ID, to, m.Subject, state))
	}
	return out
}

// Inbox returns owner's unread messages (marking them read), each labeled with
// the sender's address and role.
func (k *Kernel) Inbox(owner addr.Address) []string {
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

// Contacts returns who owner may message.
func (k *Kernel) Contacts(owner addr.Address) []addr.Address {
	return k.Caps.Contacts(owner)
}

// Introduce makes a and b mutual contacts. Root only.
func (k *Kernel) Introduce(by, a, b addr.Address) error {
	if by != addr.Root {
		return ErrNotAllowed
	}
	k.Caps.Introduce(a, b)
	return nil
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
	b.SessionID = newSessionID()
	k.Caps.AddContact(b.Addr, addr.Root) // every bubble can reach root
	k.Caps.AddContact(parent, b.Addr)    // the parent can reach its child (one-directional: no vice versa, no siblings, no ancestors)
	opts.SessionID = b.SessionID
	sess, err := k.runner.Launch(b.Addr, dir, opts)
	if err != nil {
		return "", err
	}
	k.setSession(b.Addr, sess)
	return b.Addr, nil
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

// DeleteGroup removes a group. If it had a session, that coordinator bubble is
// killed and removed. Everyone's contacts are left intact.
func (k *Kernel) DeleteGroup(name string) {
	if g, ok := k.Groups.Get(name); ok && g.Session != "" {
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

// Relaunch starts a session for an already-registered (restored) bubble and
// wires delivery, without assigning a new address. Used when rehydrating a saved
// fleet; the session resumes its prior conversation.
func (k *Kernel) Relaunch(a addr.Address, dir, persona, sessionID string) error {
	sess, err := k.runner.Launch(a, dir, runner.SpawnOpts{Persona: persona, SessionID: sessionID, Resume: true})
	if err != nil {
		return err
	}
	k.setSession(a, sess)
	return nil
}

// formatNotify renders the non-interrupting "you have mail" line typed into a
// recipient's session (no Esc, so claude queues it for its next turn). It only
// announces the message; the content is read via inbox().
func formatNotify(from addr.Address, name, subject string, unread int) string {
	f := from.String()
	if name != "" {
		f += " (" + name + ")"
	}
	// single line (no newlines) so it isn't treated as a multi-line paste
	return fmt.Sprintf("📬 New message from %s: %q — you have %d unread. Call the inbox() tool to read.", f, subject, unread)
}
