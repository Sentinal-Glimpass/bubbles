package kernel

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/registry"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// TestSessionIDConcurrentEnsureAliveAndSync drives the real interleaving that
// the fleet already has: RecoverUnread's worker pool and the keep-alive sweep
// both reach ensureAlive for the same address, while the pre-persist sweep
// (SyncSessionIDs) rewrites the same field. Before SessionID was routed through
// the registry mutex this failed under -race with a write/write and a
// read/write on registry.Bubble.SessionID.
func TestSessionIDConcurrentEnsureAliveAndSync(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0

	a, err := k.Spawn(addr.Root, "worker", t.TempDir(), runner.SpawnOpts{Persona: "worker"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	k.Reg.SetSessionID(a, "seed-session")

	// A live hook, as app.go wires: the id a bubble is actually on right now.
	var n int64
	k.CurrentSessionID = func(addr.Address) string {
		return fmt.Sprintf("live-%d", atomic.AddInt64(&n, 1))
	}

	var wg sync.WaitGroup
	const iters = 300
	for w := 0; w < 4; w++ { // several pagers-in for the SAME address
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k.EnsureAlive(a)
				if s := fr.Session(a); s != nil {
					s.Die() // page it out again so the next call relaunches
				}
			}
		}()
	}
	wg.Add(1)
	go func() { // the pre-persist sweep, concurrently rewriting the same field
		defer wg.Done()
		for i := 0; i < iters*2; i++ {
			k.SyncSessionIDs()
		}
	}()
	wg.Wait()

	if sid, ok := k.Reg.SessionID(a); !ok || sid == "" {
		t.Fatalf("session id lost after concurrent access: %q ok=%v", sid, ok)
	}
}

// TestEnsureAliveUsesOneCoherentSessionID pins that each launch decision is made
// on a SINGLE id: the resume path resumes exactly the id the registry holds, and
// the fresh path launches with the new id it then stores. A re-read at each use
// site could resume one conversation and record another.
func TestEnsureAliveUsesOneCoherentSessionID(t *testing.T) {
	t.Run("resume path", func(t *testing.T) {
		fr := runner.NewFake()
		k := New(fr)
		k.RelaunchProbe = 0
		a, err := k.Spawn(addr.Root, "worker", t.TempDir(), runner.SpawnOpts{Persona: "worker"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		k.Reg.SetSessionID(a, "stored-id")

		if k.EnsureAlive(a) == nil {
			t.Fatal("EnsureAlive returned nil")
		}
		if len(fr.Launches) != 1 {
			t.Fatalf("launches = %d want 1", len(fr.Launches))
		}
		got := fr.Launches[0]
		if !got.Opts.Resume || got.Opts.SessionID != "stored-id" {
			t.Fatalf("launch opts = %+v want Resume with stored-id", got.Opts)
		}
		if sid, _ := k.Reg.SessionID(a); sid != "stored-id" {
			t.Fatalf("stored id = %q want stored-id", sid)
		}
	})

	t.Run("fresh path", func(t *testing.T) {
		fr := runner.NewFake()
		k := New(fr)
		k.RelaunchProbe = 0
		a, err := k.Spawn(addr.Root, "worker", t.TempDir(), runner.SpawnOpts{Persona: "worker"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		// No stored id: first-ever launch must be fresh, and the id handed to the
		// runner must be the id the registry ends up holding.
		if k.EnsureAlive(a) == nil {
			t.Fatal("EnsureAlive returned nil")
		}
		if len(fr.Launches) != 1 {
			t.Fatalf("launches = %d want 1", len(fr.Launches))
		}
		got := fr.Launches[0]
		if got.Opts.Resume {
			t.Fatalf("launch opts = %+v want a fresh (non-resume) launch", got.Opts)
		}
		if got.Opts.SessionID == "" {
			t.Fatal("fresh launch got an empty session id")
		}
		sid, _ := k.Reg.SessionID(a)
		if sid != got.Opts.SessionID {
			t.Fatalf("stored id %q != launched id %q — the two uses diverged", sid, got.Opts.SessionID)
		}
	})

	t.Run("live hook wins and is recorded", func(t *testing.T) {
		fr := runner.NewFake()
		k := New(fr)
		k.RelaunchProbe = 0
		a, err := k.Spawn(addr.Root, "worker", t.TempDir(), runner.SpawnOpts{Persona: "worker"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		k.Reg.SetSessionID(a, "stale-id")
		k.CurrentSessionID = func(addr.Address) string { return "live-id" }

		if k.EnsureAlive(a) == nil {
			t.Fatal("EnsureAlive returned nil")
		}
		got := fr.Launches[0]
		if !got.Opts.Resume || got.Opts.SessionID != "live-id" {
			t.Fatalf("launch opts = %+v want Resume with live-id", got.Opts)
		}
		if sid, _ := k.Reg.SessionID(a); sid != "live-id" {
			t.Fatalf("stored id = %q want live-id", sid)
		}
	})
}

// TestSetSessionIDDoesNotBumpVersion guards the autosave contract: SyncSessionIDs
// runs immediately before a save, so marking the fleet dirty there would make
// the change-driven persist loop re-save forever.
func TestSetSessionIDDoesNotBumpVersion(t *testing.T) {
	r := registry.New()
	b := r.Add(addr.Root, "worker", "/tmp/w")
	before := r.Version()
	r.SetSessionID(b.Addr, "x")
	if after := r.Version(); after != before {
		t.Fatalf("version moved %d -> %d on SetSessionID", before, after)
	}
	if sid, ok := r.SessionID(b.Addr); !ok || sid != "x" {
		t.Fatalf("SessionID = %q ok=%v want x", sid, ok)
	}
	if _, ok := r.SessionID(addr.Address("9.9")); ok {
		t.Fatal("SessionID reported a missing bubble as present")
	}
}

// TestBubbleFieldWritesAreLocked covers the rest of the same defect: fields
// written through a registry-returned *Bubble rather than through a mutator.
//
// Two writers that must not collide:
//   - SpawnUnder sets Name/Model/Goal on a bubble Add has ALREADY published into
//     the map, so it races any concurrent mutator on that bubble.
//   - StartRoot sets root's Dir, racing any concurrent SetDir.
//
// The reader side deliberately uses only LOCKED accessors (All/Children for the
// map, then the Set*/Dir/SessionID accessors), never a deref of a returned
// pointer -- unlocked reads through Get are a separate, known issue that needs
// Registry.Get to stop handing out raw pointers.
func TestBubbleFieldWritesAreLocked(t *testing.T) {
	fr := runner.NewFake()
	k := New(fr)
	k.RelaunchProbe = 0
	dir := t.TempDir()

	var wg sync.WaitGroup
	spawned := make(chan struct{})

	wg.Add(1)
	go func() { // A: spawns, i.e. Name/Model/Goal writes on a published bubble
		defer wg.Done()
		defer close(spawned)
		for i := 0; i < 200; i++ {
			if _, err := k.SpawnUnder(addr.Root, addr.Root, "worker", dir,
				runner.SpawnOpts{Name: "n", Model: "m", Goal: "g"}); err != nil {
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // B: locked mutators over every child the spawner has published
		defer wg.Done()
		for {
			select {
			case <-spawned:
				return
			default:
			}
			for _, b := range k.Reg.Children(addr.Root) {
				k.Reg.SetName(b.Addr, "x")
				k.Reg.SetModel(b.Addr, "y")
				k.Reg.SetGoal(b.Addr, "z")
				k.Reg.SetDir(b.Addr, dir)
			}
		}
	}()

	wg.Add(1)
	go func() { // C: StartRoot's Dir write
		defer wg.Done()
		for i := 0; i < 200; i++ {
			k.sessions.Delete(addr.Root) // page root out so StartRoot runs again
			k.Reg.SetDir(addr.Root, "")  // ...and takes the "Dir unset" branch
			_ = k.StartRoot(dir)
		}
	}()

	wg.Add(1)
	go func() { // D: a concurrent SetDir on root, the partner for C's write
		defer wg.Done()
		for {
			select {
			case <-spawned:
				return
			default:
			}
			k.Reg.SetDir(addr.Root, dir)
		}
	}()
	wg.Wait()

	// Spawned bubbles survived intact and are reachable through locked reads.
	if n := len(k.Reg.Children(addr.Root)); n == 0 {
		t.Fatal("no children survived the concurrent spawn")
	}
}

// TestSpawnFieldsGoThroughMutators pins that a spawn's Name/Model/Goal actually
// land -- routing them through the mutators must not silently drop them.
func TestSpawnFieldsGoThroughMutators(t *testing.T) {
	k := New(runner.NewFake())
	a, err := k.SpawnUnder(addr.Root, addr.Root, "worker", "/tmp/w",
		runner.SpawnOpts{Name: "scout", Model: "opus", Goal: "find bugs"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	b, ok := k.Reg.Get(a)
	if !ok {
		t.Fatal("spawned bubble missing")
	}
	if b.Name != "scout" || b.Model != "opus" || b.Goal != "find bugs" || b.Persona != "worker" {
		t.Fatalf("spawn fields = %+v", b)
	}
	if d, _ := k.Reg.Dir(a); d != "/tmp/w" {
		t.Fatalf("dir = %q want /tmp/w", d)
	}
}

// TestSetDirBumpsVersion is the counterpart to TestSetSessionIDDoesNotBumpVersion:
// Dir is durable operator-set state, so a change must re-save fleet.json.
func TestSetDirBumpsVersion(t *testing.T) {
	r := registry.New()
	b := r.Add(addr.Root, "worker", "/tmp/w")
	v0 := r.Version()
	r.SetDir(b.Addr, "/tmp/other")
	if r.Version() == v0 {
		t.Fatal("SetDir must bump version so fleet.json is re-saved")
	}
	if d, ok := r.Dir(b.Addr); !ok || d != "/tmp/other" {
		t.Fatalf("Dir = %q ok=%v want /tmp/other", d, ok)
	}
	if _, ok := r.Dir(addr.Address("9.9")); ok {
		t.Fatal("Dir reported a missing bubble as present")
	}
}
