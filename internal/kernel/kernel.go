// Package kernel wires the bus, capabilities, registry, and a runner into the
// four fleet operations: Send, Contacts, Introduce, Spawn.
package kernel

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/caps"
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
	Bus   *bus.Bus
	Caps  *caps.Store
	Reg   *registry.Registry
	Store *inbox.Store

	runner   runner.Runner
	smu      sync.Mutex
	sessions map[addr.Address]runner.Session
}

// New builds a Kernel over the given runner, with root seeded.
func New(r runner.Runner) *Kernel {
	return &Kernel{
		Bus:      bus.New(),
		Caps:     caps.New(),
		Reg:      registry.New(),
		Store:    inbox.New(),
		runner:   r,
		sessions: map[addr.Address]runner.Session{},
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
func (k *Kernel) Send(from, to addr.Address, subject, body string) error {
	if !k.Caps.CanSend(from, to) {
		return ErrNotContact
	}
	fromName := ""
	if b, ok := k.Reg.Get(from); ok {
		fromName = b.Persona
	}
	k.Store.Append(inbox.Message{From: from, FromName: fromName, To: to, Subject: subject, Body: body})
	if to == addr.Root {
		_ = k.Bus.Send(bus.Message{From: from, To: to, Subject: subject, Body: body})
		return nil
	}
	if s := k.session(to); s != nil {
		unread := k.Store.UnreadCount(to)
		_, _ = s.Write([]byte(formatNotify(from, fromName, subject, unread)))
	}
	return nil
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
