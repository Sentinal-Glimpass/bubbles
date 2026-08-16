package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
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
