// Package kernel wires the bus, capabilities, registry, and a runner into the
// four fleet operations: Send, Contacts, Introduce, Spawn.
package kernel

import (
	"errors"
	"fmt"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/caps"
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
	runner runner.Runner
}

// New builds a Kernel over the given runner, with root seeded.
func New(r runner.Runner) *Kernel {
	return &Kernel{
		Bus:    bus.New(),
		Caps:   caps.New(),
		Reg:    registry.New(),
		runner: r,
	}
}

// Send delivers a message between bubbles, enforcing contacts.
func (k *Kernel) Send(from, to addr.Address, subject, body string) error {
	if !k.Caps.CanSend(from, to) {
		return ErrNotContact
	}
	return k.Bus.Send(bus.Message{From: from, To: to, Subject: subject, Body: body})
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
	k.Caps.AddContact(b.Addr, addr.Root)
	sess, err := k.runner.Launch(b.Addr, dir, opts)
	if err != nil {
		return "", err
	}
	k.Bus.Subscribe(b.Addr, func(m bus.Message) { _, _ = sess.Write([]byte(Format(m))) })
	return b.Addr, nil
}

// Relaunch starts a session for an already-registered (restored) bubble and
// wires delivery, without assigning a new address. Used when rehydrating a saved
// fleet; the session resumes its prior conversation.
func (k *Kernel) Relaunch(a addr.Address, dir, persona string) error {
	sess, err := k.runner.Launch(a, dir, runner.SpawnOpts{Persona: persona, Resume: true})
	if err != nil {
		return err
	}
	k.Bus.Subscribe(a, func(m bus.Message) { _, _ = sess.Write([]byte(Format(m))) })
	return nil
}

// Format renders a message as injected into a session's input.
func Format(m bus.Message) string {
	return fmt.Sprintf("\n[message from %s] %s — %s\n", m.From, m.Subject, m.Body)
}
