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

// Send files a message in the recipient's inbox (no interruption). If the
// recipient is root it also notifies the dashboard (blink); if urgent, it is
// additionally queued into the recipient's session for pickup on its next turn.
func (k *Kernel) Send(from, to addr.Address, subject, body string, urgent bool) error {
	if !k.Caps.CanSend(from, to) {
		return ErrNotContact
	}
	fromName := ""
	if b, ok := k.Reg.Get(from); ok {
		fromName = b.Persona
	}
	k.Store.Append(inbox.Message{From: from, FromName: fromName, To: to, Subject: subject, Body: body, Urgent: urgent})
	switch {
	case to == addr.Root:
		_ = k.Bus.Send(bus.Message{From: from, To: to, Subject: subject, Body: body})
	case urgent:
		if s := k.session(to); s != nil {
			_, _ = s.Write([]byte(formatInject(from, fromName, subject, body)))
		}
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
		tag := ""
		if m.Urgent {
			tag = " [urgent]"
		}
		out = append(out, fmt.Sprintf("[%d] from %s%s — %s: %s", m.ID, from, tag, m.Subject, m.Body))
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
	if !k.Caps.CanSpawn(by) {
		return "", ErrNotAllowed
	}
	if err := k.Caps.ConsumeSpawn(by); err != nil {
		return "", err
	}
	b := k.Reg.Add(by, persona, dir)
	b.SessionID = newSessionID()
	k.Caps.AddContact(b.Addr, addr.Root)
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

// formatInject renders an urgent message typed into a session's input (no Esc,
// so claude queues it for the next turn rather than being interrupted).
func formatInject(from addr.Address, name, subject, body string) string {
	f := from.String()
	if name != "" {
		f += " (" + name + ")"
	}
	return fmt.Sprintf("\n[message from %s] %s — %s\n", f, subject, body)
}
