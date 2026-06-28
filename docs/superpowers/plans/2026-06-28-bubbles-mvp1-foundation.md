# Bubbles MVP 1 — Plan 1: Spike + Kernel Foundation

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and fully test the Bubbles kernel — addressing, the message bus, capabilities (SMS contacts + spawn budget), the registry, and the runner abstraction — driven end-to-end by a `FakeRunner` with no real `claude`, plus a PTY spike that de-risks interrupt-delivery.

**Architecture:** One atom (a Bubble = address + session + `send`) and one verb (`send`). The kernel wires four pure-Go units (`bus`, `caps`, `registry`, `runner`) into four operations (`Send`, `Contacts`, `Introduce`, `Spawn`). The kernel never imports `claude` directly — it only talks to a `Runner` interface, so ~90% of the system is tested deterministically with a `FakeRunner`. The visible TUI and real-`claude` runner come in Plan 2, after the PTY spike tells us how delivery actually behaves.

**Tech Stack:** Go (stdlib + `github.com/creack/pty` for the spike only). Standard `go test`.

## Global Constraints

- **Go module path:** `github.com/Sentinal-Glimpass/bubbles` (adjust to your real repo path; keep all imports consistent with it).
- **Go version floor:** `go 1.22`.
- **Package layout:** kernel packages under `internal/`; the spike under `cmd/spike/`.
- **Root address is the string `"0"`** (`addr.Root`). Addresses are dot-joined segments, e.g. `0.1`, `0.1.2`.
- **Root is privileged:** root may `send` to anyone, may `spawn` without budget, and is the only address allowed to `introduce`.
- **A fresh bubble's contacts contain only root.**
- **TDD throughout** except the spike (Task 1), which is exploratory with a written findings deliverable. Commit after every task.

---

### Task 0: Toolchain & module skeleton

**Files:**
- Create: `go.mod`
- Create: `.gitignore` (already present from spec commit — leave as is)

**Interfaces:**
- Produces: a buildable Go module at `github.com/Sentinal-Glimpass/bubbles`.

- [ ] **Step 1: Install Go (if missing)**

macOS: `brew install go`
Linux: download from go.dev/dl and extract to `/usr/local/go`, add `/usr/local/go/bin` to `PATH`.

Run: `go version`
Expected: prints `go version go1.22` or newer.

- [ ] **Step 2: Initialize the module**

Run (from repo root `bubbles/`):
```bash
go mod init github.com/Sentinal-Glimpass/bubbles
```
Expected: creates `go.mod` containing `module github.com/Sentinal-Glimpass/bubbles` and a `go 1.22` line.

- [ ] **Step 3: Verify the toolchain builds an empty module**

Run: `go build ./...`
Expected: no output, exit 0 (no packages yet is fine).

- [ ] **Step 4: Commit**

```bash
git add go.mod
git commit -m "chore: initialize Go module"
```

---

### Task 1: PTY spike — de-risk interrupt-delivery

**Files:**
- Create: `cmd/spike/main.go`
- Create: `docs/superpowers/SPIKE-pty.md` (findings)

**Interfaces:**
- Produces: a written verdict on whether a live `claude` turn can be interrupted via its PTY, recorded in `SPIKE-pty.md`. This decides Plan 2's delivery design. No kernel code depends on this task.

This is a spike, not TDD. The "test" is manual observation on a machine with `claude` installed (your Mac).

- [ ] **Step 1: Add the PTY dependency**

Run:
```bash
go get github.com/creack/pty@v1.1.21
```
Expected: `go.mod` gains a `require github.com/creack/pty v1.1.21` line.

- [ ] **Step 2: Write the spike program**

Create `cmd/spike/main.go`:
```go
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
)

// spike launches a command in a PTY, types a prompt, then mid-run sends an
// interrupt byte and a follow-up, so we can observe whether interrupt-delivery
// into a live, turn-based session works.
func main() {
	cmdName := flag.String("cmd", "claude", "command to launch in a PTY")
	prompt := flag.String("prompt", "write a long haiku about the sea", "initial input")
	intByte := flag.Int("int", 0x03, "interrupt byte to send (0x03=Ctrl-C, 0x1b=ESC)")
	flag.Parse()

	c := exec.Command(*cmdName, flag.Args()...)
	ptmx, err := pty.Start(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pty start:", err)
		os.Exit(1)
	}
	defer func() { _ = ptmx.Close() }()

	go func() { _, _ = io.Copy(os.Stdout, ptmx) }()

	time.Sleep(2 * time.Second)
	fmt.Println("\n--- injecting prompt ---")
	_, _ = ptmx.Write([]byte(*prompt + "\r"))

	time.Sleep(3 * time.Second)
	fmt.Printf("\n--- sending interrupt byte 0x%02x ---\n", *intByte)
	_, _ = ptmx.Write([]byte{byte(*intByte)})

	time.Sleep(1 * time.Second)
	fmt.Println("\n--- injecting follow-up after interrupt ---")
	_, _ = ptmx.Write([]byte("actually, write about mountains instead\r"))

	time.Sleep(6 * time.Second)
	fmt.Println("\n--- spike done ---")
}
```

- [ ] **Step 3: Run the spike against `claude` (on a machine that has it)**

Run:
```bash
go run ./cmd/spike -cmd claude
# If Ctrl-C doesn't interrupt, try ESC:
go run ./cmd/spike -cmd claude -int 0x1b
```
Observe: Does the prompt inject? Does the session start working? Does the interrupt byte stop the current turn? Does the follow-up land as a new instruction?

(If `claude` isn't available where you run this, run `-cmd bash` first just to confirm PTY plumbing works: you should see a shell, the injected command run, and Ctrl-C behave.)

- [ ] **Step 4: Record findings**

Create `docs/superpowers/SPIKE-pty.md`:
```markdown
# Spike: PTY interrupt-delivery into a live `claude` session

**Date:**
**Machine / claude version:**

## Observations
- Prompt injection works: yes / no
- Session begins working: yes / no
- Interrupt byte that stops a turn: 0x03 (Ctrl-C) / 0x1b (ESC) / none found
- Follow-up after interrupt lands as new instruction: yes / no

## Verdict
- [ ] Interrupt-delivery is viable → Plan 2 uses interrupt injection.
- [ ] Interrupt unreliable → Plan 2 adopts the Stop-hook turn-boundary fallback
      (a Claude Code Stop hook drains the inbox at end of each turn).

## Notes
(anything surprising — escape sequences, timing, paste-mode, etc.)
```
Fill it in from Step 3.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/spike/main.go docs/superpowers/SPIKE-pty.md
git commit -m "spike: PTY interrupt-delivery into a live claude session"
```

---

### Task 2: `addr` — hierarchical addresses

**Files:**
- Create: `internal/addr/addr.go`
- Test: `internal/addr/addr_test.go`

**Interfaces:**
- Produces:
  - `type Address string`, const `Root Address = "0"`, var `ErrInvalid error`
  - `Parse(s string) (Address, error)`
  - `(Address) String() string`, `(Address) Child(seg string) Address`
  - `(Address) Parent() (Address, bool)`, `(Address) IsRoot() bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/addr/addr_test.go`:
```go
package addr

import "testing"

func TestParse(t *testing.T) {
	good := []string{"0", "0.1", "0.1.2"}
	for _, s := range good {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{"", "1", "0.", ".0", "0..1", "x"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", s)
		}
	}
}

func TestChildParent(t *testing.T) {
	a := Root.Child("1").Child("2")
	if a.String() != "0.1.2" {
		t.Fatalf("got %q want 0.1.2", a)
	}
	p, ok := a.Parent()
	if !ok || p != Address("0.1") {
		t.Fatalf("Parent() = %q,%v want 0.1,true", p, ok)
	}
	if _, ok := Root.Parent(); ok {
		t.Fatalf("Root.Parent() ok = true, want false")
	}
}

func TestIsRoot(t *testing.T) {
	if !Root.IsRoot() {
		t.Fatal("Root.IsRoot() = false")
	}
	if Root.Child("1").IsRoot() {
		t.Fatal("child IsRoot() = true")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/addr/ -v`
Expected: FAIL — `undefined: Address` / build error.

- [ ] **Step 3: Implement**

Create `internal/addr/addr.go`:
```go
// Package addr defines hierarchical bubble addresses like "0", "0.1", "0.1.2".
package addr

import (
	"errors"
	"strings"
)

// Address is an immutable hierarchical address. Compare with ==.
type Address string

// Root is the human/operator address.
const Root Address = "0"

// ErrInvalid is returned by Parse for malformed addresses.
var ErrInvalid = errors.New("addr: invalid address")

// Parse validates s: "0" optionally followed by ".N" segments, each non-empty.
func Parse(s string) (Address, error) {
	if s == "" {
		return "", ErrInvalid
	}
	parts := strings.Split(s, ".")
	if parts[0] != "0" {
		return "", ErrInvalid
	}
	for _, p := range parts {
		if p == "" {
			return "", ErrInvalid
		}
	}
	return Address(s), nil
}

func (a Address) String() string { return string(a) }

// Child returns a new address with seg appended.
func (a Address) Child(seg string) Address { return Address(string(a) + "." + seg) }

// Parent returns the parent and true, or ("", false) for root.
func (a Address) Parent() (Address, bool) {
	i := strings.LastIndex(string(a), ".")
	if i < 0 {
		return "", false
	}
	return a[:i], true
}

// IsRoot reports whether a is the root address.
func (a Address) IsRoot() bool { return a == Root }
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/addr/ -v`
Expected: PASS (TestParse, TestChildParent, TestIsRoot).

- [ ] **Step 5: Commit**

```bash
git add internal/addr/
git commit -m "feat(addr): hierarchical bubble addresses"
```

---

### Task 3: `bus` — synchronous address-routed messaging

**Files:**
- Create: `internal/bus/bus.go`
- Test: `internal/bus/bus_test.go`

**Interfaces:**
- Consumes: `addr.Address`.
- Produces:
  - `type Message struct { From, To addr.Address; Subject, Body string }`
  - `type Handler func(Message)`
  - `var ErrNoInbox error`
  - `New() *Bus`, `(*Bus) Subscribe(addr.Address, Handler)`, `(*Bus) Unsubscribe(addr.Address)`, `(*Bus) Send(Message) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/bus/bus_test.go`:
```go
package bus

import (
	"errors"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestSendDelivers(t *testing.T) {
	b := New()
	var got Message
	b.Subscribe(addr.Root, func(m Message) { got = m })
	err := b.Send(Message{From: "0.1", To: addr.Root, Subject: "hi", Body: "there"})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if got.From != "0.1" || got.Subject != "hi" {
		t.Fatalf("handler got %+v", got)
	}
}

func TestSendNoInbox(t *testing.T) {
	b := New()
	err := b.Send(Message{To: "0.9"})
	if !errors.Is(err, ErrNoInbox) {
		t.Fatalf("got %v want ErrNoInbox", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/bus/ -v`
Expected: FAIL — build error (`undefined: New`).

- [ ] **Step 3: Implement**

Create `internal/bus/bus.go`:
```go
// Package bus is an in-memory, synchronous, address-routed message bus.
package bus

import (
	"errors"
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// Message is a single piece of mail between bubbles.
type Message struct {
	From    addr.Address
	To      addr.Address
	Subject string
	Body    string
}

// Handler receives messages delivered to a subscribed address.
type Handler func(Message)

// ErrNoInbox is returned by Send when the destination has no subscriber.
var ErrNoInbox = errors.New("bus: no inbox for address")

// Bus routes messages to per-address handlers (one inbox per address).
type Bus struct {
	mu       sync.Mutex
	handlers map[addr.Address]Handler
}

func New() *Bus { return &Bus{handlers: map[addr.Address]Handler{}} }

// Subscribe sets the inbox handler for an address, replacing any existing one.
func (b *Bus) Subscribe(a addr.Address, h Handler) {
	b.mu.Lock()
	b.handlers[a] = h
	b.mu.Unlock()
}

func (b *Bus) Unsubscribe(a addr.Address) {
	b.mu.Lock()
	delete(b.handlers, a)
	b.mu.Unlock()
}

// Send routes m to m.To's handler, called outside the lock so a handler may
// itself Send. Returns ErrNoInbox if nothing is subscribed.
func (b *Bus) Send(m Message) error {
	b.mu.Lock()
	h, ok := b.handlers[m.To]
	b.mu.Unlock()
	if !ok {
		return ErrNoInbox
	}
	h(m)
	return nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/bus/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bus/
git commit -m "feat(bus): synchronous address-routed message bus"
```

---

### Task 4: `caps` — contacts + spawn budget

**Files:**
- Create: `internal/caps/caps.go`
- Test: `internal/caps/caps_test.go`

**Interfaces:**
- Consumes: `addr.Address`, `addr.Root`.
- Produces:
  - `var ErrNoBudget error`
  - `New() *Store`
  - `(*Store) AddContact(owner, contact addr.Address)`
  - `(*Store) Introduce(a, b addr.Address)`
  - `(*Store) CanSend(from, to addr.Address) bool`
  - `(*Store) Contacts(owner addr.Address) []addr.Address`
  - `(*Store) GrantSpawn(owner addr.Address, n int)`
  - `(*Store) CanSpawn(owner addr.Address) bool`
  - `(*Store) ConsumeSpawn(owner addr.Address) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/caps/caps_test.go`:
```go
package caps

import (
	"errors"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestContactsAndSend(t *testing.T) {
	s := New()
	s.AddContact("0.1", addr.Root) // fresh bubble knows only root
	if !s.CanSend("0.1", addr.Root) {
		t.Fatal("0.1 should reach root")
	}
	if s.CanSend("0.1", "0.2") {
		t.Fatal("0.1 should not reach 0.2 before introduction")
	}
	s.Introduce("0.1", "0.2")
	if !s.CanSend("0.1", "0.2") || !s.CanSend("0.2", "0.1") {
		t.Fatal("introduction should make contact mutual")
	}
}

func TestRootCanSendAnyone(t *testing.T) {
	s := New()
	if !s.CanSend(addr.Root, "0.7") {
		t.Fatal("root should reach anyone")
	}
}

func TestSpawnBudget(t *testing.T) {
	s := New()
	if !s.CanSpawn(addr.Root) {
		t.Fatal("root should always spawn")
	}
	if s.CanSpawn("0.1") {
		t.Fatal("ungranted bubble should not spawn")
	}
	s.GrantSpawn("0.1", 1)
	if !s.CanSpawn("0.1") {
		t.Fatal("granted bubble should spawn")
	}
	if err := s.ConsumeSpawn("0.1"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := s.ConsumeSpawn("0.1"); !errors.Is(err, ErrNoBudget) {
		t.Fatalf("second consume got %v want ErrNoBudget", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/caps/ -v`
Expected: FAIL — build error (`undefined: New`).

- [ ] **Step 3: Implement**

Create `internal/caps/caps.go`:
```go
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
	spawn    map[addr.Address]int
}

func New() *Store {
	return &Store{
		contacts: map[addr.Address]map[addr.Address]bool{},
		spawn:    map[addr.Address]int{},
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

// CanSpawn reports whether owner may spawn. Root always may.
func (s *Store) CanSpawn(owner addr.Address) bool {
	if owner == addr.Root {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawn[owner] > 0
}

// ConsumeSpawn decrements owner's budget. Root is unlimited (no-op).
func (s *Store) ConsumeSpawn(owner addr.Address) error {
	if owner == addr.Root {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spawn[owner] <= 0 {
		return ErrNoBudget
	}
	s.spawn[owner]--
	return nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/caps/ -v`
Expected: PASS (TestContactsAndSend, TestRootCanSendAnyone, TestSpawnBudget).

- [ ] **Step 5: Commit**

```bash
git add internal/caps/
git commit -m "feat(caps): SMS contacts and spawn budget capabilities"
```

---

### Task 5: `registry` — bubble lifecycle & child addressing

**Files:**
- Create: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

**Interfaces:**
- Consumes: `addr.Address`, `addr.Root`.
- Produces:
  - `type Status string`; consts `Idle, Working, Waiting, Done Status`
  - `type Bubble struct { Addr addr.Address; Persona string; Status Status; Parent addr.Address; Dir string }`
  - `New() *Registry`
  - `(*Registry) Add(parent addr.Address, persona, dir string) *Bubble`
  - `(*Registry) Get(addr.Address) (*Bubble, bool)`
  - `(*Registry) SetStatus(addr.Address, Status)`
  - `(*Registry) Children(addr.Address) []*Bubble`

- [ ] **Step 1: Write the failing tests**

Create `internal/registry/registry_test.go`:
```go
package registry

import (
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestRootSeeded(t *testing.T) {
	r := New()
	b, ok := r.Get(addr.Root)
	if !ok || b.Persona != "root" {
		t.Fatalf("root not seeded: %+v ok=%v", b, ok)
	}
}

func TestAddAssignsAddresses(t *testing.T) {
	r := New()
	a1 := r.Add(addr.Root, "scout", "/tmp/scout")
	a2 := r.Add(addr.Root, "docs", "/tmp/docs")
	if a1.Addr != "0.1" || a2.Addr != "0.2" {
		t.Fatalf("got %q,%q want 0.1,0.2", a1.Addr, a2.Addr)
	}
	nested := r.Add(a1.Addr, "helper", "/tmp/h")
	if nested.Addr != "0.1.1" {
		t.Fatalf("nested = %q want 0.1.1", nested.Addr)
	}
}

func TestStatusAndChildren(t *testing.T) {
	r := New()
	a1 := r.Add(addr.Root, "scout", "")
	r.Add(addr.Root, "docs", "")
	r.SetStatus(a1.Addr, Done)
	if b, _ := r.Get(a1.Addr); b.Status != Done {
		t.Fatalf("status = %q want done", b.Status)
	}
	if got := len(r.Children(addr.Root)); got != 2 {
		t.Fatalf("root children = %d want 2", got)
	}
	if _, ok := r.Get("0.9"); ok {
		t.Fatal("Get unknown should be false")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/registry/ -v`
Expected: FAIL — build error.

- [ ] **Step 3: Implement**

Create `internal/registry/registry.go`:
```go
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
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/registry/ -v`
Expected: PASS (TestRootSeeded, TestAddAssignsAddresses, TestStatusAndChildren).

- [ ] **Step 5: Commit**

```bash
git add internal/registry/
git commit -m "feat(registry): bubble lifecycle and child addressing"
```

---

### Task 6: `runner` — interface + FakeRunner

**Files:**
- Create: `internal/runner/runner.go`
- Create: `internal/runner/fake.go`
- Test: `internal/runner/fake_test.go`

**Interfaces:**
- Consumes: `addr.Address`.
- Produces:
  - `type SpawnOpts struct { Persona, Goal string }`
  - `type Session interface { Write([]byte) (int, error); Close() error }`
  - `type Runner interface { Launch(addr.Address, string, SpawnOpts) (Session, error); Kill(addr.Address) error }`
  - `type FakeSession struct{...}` with `Write`, `Close`, `Written() string`, `Closed() bool`
  - `NewFake() *FakeRunner`; `(*FakeRunner) Launch/Kill`; `(*FakeRunner) Session(addr.Address) *FakeSession`

- [ ] **Step 1: Write the failing tests**

Create `internal/runner/fake_test.go`:
```go
package runner

import "testing"

func TestFakeRunnerRecordsWrites(t *testing.T) {
	r := NewFake()
	sess, err := r.Launch("0.1", "/tmp/x", SpawnOpts{Persona: "scout"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := sess.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := r.Session("0.1").Written(); got != "hello" {
		t.Fatalf("Written = %q want hello", got)
	}
}

func TestFakeRunnerKillCloses(t *testing.T) {
	r := NewFake()
	_, _ = r.Launch("0.1", "", SpawnOpts{})
	if err := r.Kill("0.1"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !r.Session("0.1").Closed() {
		t.Fatal("session not closed after Kill")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runner/ -v`
Expected: FAIL — build error.

- [ ] **Step 3: Implement the interface**

Create `internal/runner/runner.go`:
```go
// Package runner launches and kills bubble sessions behind an interface, so the
// kernel never depends on real claude. LocalRunner/SSHRunner come in Plan 2.
package runner

import "github.com/Sentinal-Glimpass/bubbles/internal/addr"

// SpawnOpts configures a launched session.
type SpawnOpts struct {
	Persona string
	Goal    string
}

// Session is a running agent we can inject input into (message delivery).
type Session interface {
	Write(p []byte) (int, error)
	Close() error
}

// Runner launches and kills sessions by address.
type Runner interface {
	Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error)
	Kill(a addr.Address) error
}
```

- [ ] **Step 4: Implement the fake**

Create `internal/runner/fake.go`:
```go
package runner

import (
	"sync"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// FakeSession records everything written to it (for tests).
type FakeSession struct {
	mu      sync.Mutex
	written []byte
	closed  bool
}

func (s *FakeSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *FakeSession) Written() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.written)
}

func (s *FakeSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// FakeRunner is an in-memory Runner for tests — no real processes.
type FakeRunner struct {
	mu       sync.Mutex
	sessions map[addr.Address]*FakeSession
}

func NewFake() *FakeRunner {
	return &FakeRunner{sessions: map[addr.Address]*FakeSession{}}
}

func (r *FakeRunner) Launch(a addr.Address, dir string, opts SpawnOpts) (Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &FakeSession{}
	r.sessions[a] = s
	return s, nil
}

func (r *FakeRunner) Kill(a addr.Address) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[a]; ok {
		s.closed = true
	}
	return nil
}

// Session returns the FakeSession launched for a (test helper).
func (r *FakeRunner) Session(a addr.Address) *FakeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[a]
}
```

- [ ] **Step 5: Run to verify pass**

Run: `go test ./internal/runner/ -v`
Expected: PASS (TestFakeRunnerRecordsWrites, TestFakeRunnerKillCloses).

- [ ] **Step 6: Commit**

```bash
git add internal/runner/
git commit -m "feat(runner): Runner interface and in-memory FakeRunner"
```

---

### Task 7: `kernel` — wire it all + end-to-end test

**Files:**
- Create: `internal/kernel/kernel.go`
- Test: `internal/kernel/kernel_test.go`

**Interfaces:**
- Consumes: `addr`, `bus`, `caps`, `registry`, `runner` (all above).
- Produces:
  - `var ErrNotContact, ErrNotAllowed error`
  - `type Kernel struct { Bus *bus.Bus; Caps *caps.Store; Reg *registry.Registry; ... }`
  - `New(r runner.Runner) *Kernel`
  - `(*Kernel) Send(from, to addr.Address, subject, body string) error`
  - `(*Kernel) Contacts(owner addr.Address) []addr.Address`
  - `(*Kernel) Introduce(by, a, b addr.Address) error`
  - `(*Kernel) Spawn(by addr.Address, persona, dir string, opts runner.SpawnOpts) (addr.Address, error)`
  - `Format(bus.Message) string`

- [ ] **Step 1: Write the failing end-to-end test**

Create `internal/kernel/kernel_test.go`:
```go
package kernel

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/bus"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

func TestFleetEndToEnd(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)

	// Root's inbox captures pings.
	var pings []bus.Message
	k.Bus.Subscribe(addr.Root, func(m bus.Message) { pings = append(pings, m) })

	// Spawn two workers under root.
	scout, err := k.Spawn(addr.Root, "scout", "/tmp/scout", runner.SpawnOpts{Persona: "scout"})
	if err != nil {
		t.Fatalf("spawn scout: %v", err)
	}
	if scout != "0.1" {
		t.Fatalf("scout addr = %q want 0.1", scout)
	}
	refactor, err := k.Spawn(addr.Root, "refactor", "/tmp/refactor", runner.SpawnOpts{Persona: "refactor"})
	if err != nil {
		t.Fatalf("spawn refactor: %v", err)
	}

	// Worker pings root.
	if err := k.Send(scout, addr.Root, "found 3 bugs", "details"); err != nil {
		t.Fatalf("scout->root: %v", err)
	}
	if len(pings) != 1 || pings[0].From != scout {
		t.Fatalf("pings = %+v", pings)
	}

	// Workers can't talk before introduction.
	if err := k.Send(scout, refactor, "hi", ""); !errors.Is(err, ErrNotContact) {
		t.Fatalf("got %v want ErrNotContact", err)
	}

	// Root introduces them, then they text each other (delivery is injected).
	if err := k.Introduce(addr.Root, scout, refactor); err != nil {
		t.Fatalf("introduce: %v", err)
	}
	if err := k.Send(scout, refactor, "take the API layer", "thanks"); err != nil {
		t.Fatalf("scout->refactor: %v", err)
	}
	if got := fr.Session(refactor).Written(); !strings.Contains(got, "from "+scout.String()) ||
		!strings.Contains(got, "take the API layer") {
		t.Fatalf("refactor session got %q", got)
	}
	if err := k.Send(refactor, scout, "on it", ""); err != nil {
		t.Fatalf("refactor->scout: %v", err)
	}
	if got := fr.Session(scout).Written(); !strings.Contains(got, "from "+refactor.String()) {
		t.Fatalf("scout session got %q", got)
	}
}

func TestIntroduceRootOnly(t *testing.T) {
	k := New(runner.NewFake())
	if err := k.Introduce("0.1", "0.2", "0.3"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v want ErrNotAllowed", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/kernel/ -v`
Expected: FAIL — build error (`undefined: New`).

- [ ] **Step 3: Implement**

Create `internal/kernel/kernel.go`:
```go
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

// Format renders a message as injected into a session's input.
func Format(m bus.Message) string {
	return fmt.Sprintf("\n[message from %s] %s — %s\n", m.From, m.Subject, m.Body)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/kernel/ -v`
Expected: PASS (TestFleetEndToEnd, TestIntroduceRootOnly).

- [ ] **Step 5: Run the whole suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/kernel/
git commit -m "feat(kernel): wire bus+caps+registry+runner into fleet operations"
```

---

## What Plan 1 delivers

A fully-tested SMS+spawn kernel: spawn workers, root-seeded contacts, `introduce` to make peers, contact-enforced `send`, and message delivery injected into sessions — all proven end-to-end with `FakeRunner` (no `claude`, no tokens, no network) — plus a PTY spike verdict that unblocks Plan 2.

## Plan 2 (next, after the spike)

`LocalRunner` launching real `claude` in a PTY (delivery per the spike verdict), the MCP stdio server exposing `send`/`contacts`/`spawn`/`introduce` to sessions, the `bubble-citizen` skill, the Bubbletea TUI (zoomable tree, blink pings, dive-in/`esc`), workspace config, and a single-binary build. Written once the spike tells us whether delivery is interrupt-based or Stop-hook-based.
```
