package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
	"github.com/Sentinal-Glimpass/bubbles/internal/supervisor"
)

// appSource returns main's source, once, for the ordering guards below.
func appSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	return string(src)
}

// sourceIndex locates a startup step in main and fails loudly if the step has
// been renamed away or now appears twice — either would silently turn these
// guards into no-ops, which is the one way an ordering test can rot.
func sourceIndex(t *testing.T, src, needle string) int {
	t.Helper()
	i := strings.Index(src, needle)
	if i < 0 {
		t.Fatalf("app.go no longer contains %q — this guard must be updated deliberately, not deleted", needle)
	}
	if j := strings.Index(src[i+len(needle):], needle); j >= 0 {
		t.Fatalf("%q appears more than once in app.go; the guard cannot tell which one runs first", needle)
	}
	return i
}

// startupLoads are every step that installs persisted state into the kernel;
// startupBinds are every step that starts accepting traffic. THE RULE IS THAT
// EVERY LOAD RUNS ABOVE EVERY BIND, and this table is how a new load or a new
// listener gets covered: add it here.
var (
	startupLoads = []struct{ what, needle string }{
		{"the fleet registry", "restoreFleet(baseDir, k)"},
		{"the session-id repair", "reconcileSessionIDs(k, home,"},
		{"the message store", "loadInbox(baseDir, k)"},
		{"the schedule store", "loadSchedules(baseDir, k)"},
		{"the task ledger", "loadTasks(baseDir, k)"},
	}
	startupBinds = []struct{ what, needle string }{
		{"the webhook server", "startWebhookServer(k)"},
		{"the IPC socket", "ipc.Serve(sock,"},
	}
	// startupSupervisor is the same rule from the other side. A listener is not
	// the only way state gets touched before it is loaded: the phaseBoot checks
	// are started by main itself, and inbox-drain, recover-unread and schedules
	// read exactly the stores the loads are about to REPLACE. Registration seeds
	// each check's first due time, and the driver is what actually executes
	// them, so both belong below the loads.
	startupSupervisor = []struct{ what, needle string }{
		{"the phaseBoot checks are registered", "registerPhase(checkReg, checks, phaseBoot)"},
		{"the supervisor driver is started", "go runChecks(checkCtx, checkReg, supervisorTick)"},
	}
)

// TestPersistedStoresAreLoadedBeforeListenersBind pins the startup ORDERING,
// which is the only thing that makes the loads safe.
//
// startWebhookServer and ipc.Serve accept traffic the moment they are bound,
// and that traffic mutates exactly the state the loads are about to install.
// Bind either of them first and there are two shapes of loss: a delivery in
// that window launches a bubble on registry state that has not been restored or
// repaired yet, and — worse — every store's Load REPLACES its contents
// wholesale, so a message, schedule or task that arrives in the window is
// overwritten by the file rather than merged with it, and is simply gone.
//
// Asserted against the source because the property IS the order of statements
// in main, which no runtime seam can observe without becoming a second copy of
// the thing under test. Ordering is used deliberately in preference to a
// "loaded yet?" gate: an unbound listener cannot deliver at all, whereas a gate
// is more state to get wrong on the next delivery path someone adds.
func TestPersistedStoresAreLoadedBeforeListenersBind(t *testing.T) {
	src := appSource(t)
	for _, load := range startupLoads {
		li := sourceIndex(t, src, load.needle)
		for _, bind := range startupBinds {
			if li > sourceIndex(t, src, bind.needle) {
				t.Errorf("%s is loaded AFTER %s binds: traffic accepted in that window is destroyed by the load that follows it (every store's Load replaces its contents wholesale)",
					load.what, bind.what)
			}
		}
	}
	// The savers must still come after the loads: that is what the phaseAfterLoad
	// NOTE in app.go protects, and moving loads earlier must not have inverted it.
	last := 0
	for _, load := range startupLoads {
		if i := sourceIndex(t, src, load.needle); i > last {
			last = i
		}
	}
	if savers := sourceIndex(t, src, "registerPhase(checkReg, checks, phaseAfterLoad)"); savers < last {
		t.Error("the phaseAfterLoad savers are registered BEFORE a load completes: a saver tick can write empty in-memory state over a persisted file")
	}
}

// TestMessageArrivingAtTheBindWindowIsNeverDropped is the same rule, executed
// rather than read.
//
// The order of the two steps comes FROM app.go, so this test runs whatever
// sequence main actually runs: with the binds first it delivers before loading
// (the old, lossy sequence), with the loads first it delivers after. The
// assertion is the repo's oldest law and does not care which: no message is
// ever dropped, and UnreadCount stays truthful.
//
// inbox.Store.Load rebuilds s.all from the file, so in the lossy order the
// delivered message is not merged — it is overwritten, and nothing anywhere
// notices: the sender was told it was delivered and UnreadCount never counted
// it. There is no self-healing fallback for this the way a bad session id at
// least fails its resume and starts fresh.
func TestMessageArrivingAtTheBindWindowIsNeverDropped(t *testing.T) {
	base := t.TempDir()

	// A daemon that has run before: one bubble, one message already persisted.
	k0 := kernel.New(runner.NewFake())
	k0.RelaunchProbe = 0
	a, err := k0.Spawn(addr.Root, "w", filepath.Join(base, "w"), runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := k0.Send(addr.Root, a, "from-the-previous-run", "body", 0, true); err != nil {
		t.Fatalf("seed send: %v", err)
	}
	if err := saveFleet(base, k0, map[int]addr.Address{}); err != nil {
		t.Fatalf("save fleet: %v", err)
	}
	if err := saveInbox(base, k0); err != nil {
		t.Fatalf("save inbox: %v", err)
	}

	// The process restarts.
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	restoreFleet(base, k)

	deliver := func() { // what a bound listener does with an inbound message
		if _, err := k.Send(addr.Root, a, "arrived-in-the-window", "body", 0, true); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	load := func() { loadInbox(base, k) }

	// Take the order from main itself.
	src := appSource(t)
	loadAt := sourceIndex(t, src, "loadInbox(baseDir, k)")
	bindAt := sourceIndex(t, src, "ipc.Serve(sock,")
	if bindAt < loadAt {
		deliver() // the listener is up first: this is the window
		load()
	} else {
		load()
		deliver()
	}

	msgs, _ := k.Store.Snapshot()
	found, seeded := false, false
	wantUnread := 0
	for _, m := range msgs {
		switch m.Subject {
		case "arrived-in-the-window":
			found = true
			if m.Read {
				t.Error("the delivered message came back marked read")
			}
		case "from-the-previous-run":
			seeded = true
		}
		if m.To == a && !m.Read {
			wantUnread++
		}
	}
	if !found {
		t.Fatalf("the message delivered during startup is GONE (store holds %d message(s)): a load that replaces the store wholesale must never run after a listener is bound", len(msgs))
	}
	if !seeded {
		t.Error("the previously persisted message did not survive either — the load itself is broken")
	}
	if got := k.Store.UnreadCount(a); got != wantUnread || got == 0 {
		t.Fatalf("UnreadCount(%s) = %d, want %d (the count must agree with the store it counts)", a, got, wantUnread)
	}
}

// TestBackgroundChecksStartAfterEveryLoad is EVERY LOAD ABOVE EVERY BIND applied
// to the supervisor, which is the other thing in main that starts touching the
// stores on its own.
//
// The phaseBoot inventory is not passive: inbox-drain reaches EnsureAlive and
// can page a cold bubble in over its mail, recover-unread re-nudges bubbles from
// what UnreadCount says, and schedules fires durable wakes. All three read a
// store whose Load REPLACES its contents wholesale, so a tick above the loads
// acts on an EMPTY store and then has its whole view swapped out underneath it —
// a wake that should have fired is skipped and rearmed from nothing, and mail
// that was on disk the entire time goes un-drained until the next slow tick.
//
// Ordering again in preference to a "loaded yet?" gate, for the same reason the
// block in app.go gives: a driver that has not been started cannot run anything,
// whereas a gate is another piece of state to get wrong in the next check
// somebody adds.
func TestBackgroundChecksStartAfterEveryLoad(t *testing.T) {
	src := appSource(t)
	for _, load := range startupLoads {
		li := sourceIndex(t, src, load.needle)
		for _, sup := range startupSupervisor {
			if li > sourceIndex(t, src, sup.needle) {
				t.Errorf("%s is loaded AFTER %s: a phaseBoot check can run against the pre-load store and then have it replaced underneath it (every store's Load replaces its contents wholesale)",
					load.what, sup.what)
			}
		}
	}
	// The savers stay below the boot checks: phaseAfterLoad exists to keep a
	// saver tick from writing empty in-memory state over a persisted file, and
	// moving the boot registration down must not have collapsed the two phases
	// into one registration point.
	boot := sourceIndex(t, src, "registerPhase(checkReg, checks, phaseBoot)")
	if after := sourceIndex(t, src, "registerPhase(checkReg, checks, phaseAfterLoad)"); after < boot {
		t.Error("the phaseAfterLoad savers are registered BEFORE the phaseBoot checks: the two phases exist precisely to be ordered")
	}
}

// TestABackgroundCheckNeverSeesThePreLoadStore is that rule executed rather than
// read, in the shape of TestMessageArrivingAtTheBindWindowIsNeverDropped.
//
// The order of the two steps comes FROM app.go, so this runs whatever sequence
// main actually runs: start the supervisor above the loads and a check observes
// the empty store, start it below and the same check observes the persisted
// mail. The stand-in check only reads the store, which is the part of
// inbox-drain / recover-unread / schedules that matters here — what they then DO
// (nudge a bubble, wake one on a schedule) is decided entirely by what they saw.
func TestABackgroundCheckNeverSeesThePreLoadStore(t *testing.T) {
	base := t.TempDir()

	// A daemon that has run before: one bubble with one unread message on disk.
	k0 := kernel.New(runner.NewFake())
	k0.RelaunchProbe = 0
	a, err := k0.Spawn(addr.Root, "w", filepath.Join(base, "w"), runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := k0.Send(addr.Root, a, "from-the-previous-run", "body", 0, true); err != nil {
		t.Fatalf("seed send: %v", err)
	}
	if err := saveFleet(base, k0, map[int]addr.Address{}); err != nil {
		t.Fatalf("save fleet: %v", err)
	}
	if err := saveInbox(base, k0); err != nil {
		t.Fatalf("save inbox: %v", err)
	}

	// The process restarts.
	k := kernel.New(runner.NewFake())
	k.RelaunchProbe = 0
	restoreFleet(base, k)

	// What a phaseBoot check that reads the message store sees on its FIRST tick.
	// Buffered and non-blocking so the check keeps its real cadence afterwards
	// instead of becoming a one-shot that could never overlap the load.
	firstSight := make(chan int, 1)
	reg := supervisor.New(time.Now)
	if err := reg.Register(supervisor.Check{Name: "reads-the-message-store", Every: time.Millisecond, Fn: func(context.Context) error {
		msgs, _ := k.Store.Snapshot()
		select {
		case firstSight <- len(msgs):
		default:
		}
		return nil
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// startChecks is what main's `go runChecks(...)` does, and it does not return
	// until a check has actually run — so "the supervisor was started first"
	// means a check really did observe the store, not merely that a goroutine
	// existed. The real driver is used rather than a hand-rolled loop.
	startChecks := func() int {
		t.Helper()
		go runChecks(ctx, reg, time.Millisecond)
		select {
		case n := <-firstSight:
			return n
		case <-time.After(10 * time.Second):
			t.Fatal("the background check never ran")
			return -1
		}
	}
	load := func() { loadInbox(base, k) }

	// Take the order from main itself.
	src := appSource(t)
	loadAt := sourceIndex(t, src, "loadInbox(baseDir, k)")
	startAt := sourceIndex(t, src, "go runChecks(checkCtx, checkReg, supervisorTick)")
	var observed int
	if startAt < loadAt {
		observed = startChecks() // the supervisor is up first: this is the window
		load()
	} else {
		load()
		observed = startChecks()
	}

	if observed == 0 {
		t.Fatalf("a background check ran against an EMPTY message store while %d message(s) sat on disk unloaded: inbox-drain would find no mail to drain, recover-unread nobody to re-nudge, and schedules nothing due — and then the load replaces the store underneath them", 1)
	}
	if observed != 1 {
		t.Fatalf("the check saw %d message(s), want the 1 that was persisted", observed)
	}
	// And the load still did its job: the guard must not be satisfiable by
	// simply never loading.
	msgs, _ := k.Store.Snapshot()
	if len(msgs) != 1 || msgs[0].Subject != "from-the-previous-run" {
		t.Fatalf("after the load the store holds %d message(s), want the 1 persisted one", len(msgs))
	}
	if got := k.Store.UnreadCount(a); got != 1 {
		t.Fatalf("UnreadCount(%s) = %d, want 1 (the count must agree with the store it counts)", a, got)
	}
}
