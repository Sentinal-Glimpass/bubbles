package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
	"github.com/Sentinal-Glimpass/bubbles/internal/kernel"
	"github.com/Sentinal-Glimpass/bubbles/internal/runner"
)

// repairFixture is a one-bubble fleet with a private fake HOME, so convPath
// resolves inside a temp dir and a transcript can be planted (or withheld).
type repairFixture struct {
	k    *kernel.Kernel
	fr   *runner.FakeRunner
	a    addr.Address
	home string
	dir  string
	log  strings.Builder
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	fr := runner.NewFake()
	k := kernel.New(fr)
	k.RelaunchProbe = 0
	a, err := k.Spawn(addr.Root, "w", dir, runner.SpawnOpts{Persona: "w"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return &repairFixture{k: k, fr: fr, a: a, home: home, dir: dir}
}

func (f *repairFixture) logf(format string, a ...any) { fmt.Fprintf(&f.log, format, a...) }

func (f *repairFixture) run() int { return reconcileSessionIDs(f.k, f.home, f.logf) }

// plant writes a transcript for sid and returns its path.
func (f *repairFixture) plant(t *testing.T, sid, body string) string {
	t.Helper()
	p := convPath(f.home, f.dir, sid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReconcileClearsPhantomSessionID: a stored id naming a transcript that
// does not exist is the state four bubbles were found in. It must be cleared —
// so the bubble starts a FRESH conversation instead of resuming a ghost — and
// it must be announced with enough detail (address and id) to go find the real
// conversation in a backup.
func TestReconcileClearsPhantomSessionID(t *testing.T) {
	f := newRepairFixture(t)
	f.k.Reg.RecordSessionID(f.a, "sess-that-never-existed")
	// A real transcript belonging to a DIFFERENT conversation, sitting in the
	// same project folder: the reconciler must not "repair" by adopting it.
	other := f.plant(t, "somebody-elses-session", "{\"type\":\"assistant\"}\n")
	otherBefore, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}

	if n := f.run(); n != 1 {
		t.Fatalf("cleared = %d want 1", n)
	}
	if sid, _ := f.k.Reg.SessionID(f.a); sid != "" {
		t.Fatalf("session id = %q want empty — a phantom id must be cleared, never replaced by a guess", sid)
	}
	line := f.log.String()
	for _, want := range []string{string(f.a), "sess-that-never-existed"} {
		if !strings.Contains(line, want) {
			t.Errorf("reconcile log is missing %q; got %q", want, line)
		}
	}
	// No transcript was touched: the unrelated one is byte-identical and the
	// phantom's own path was not created.
	after, err := os.ReadFile(other)
	if err != nil || string(after) != string(otherBefore) {
		t.Fatalf("an unrelated transcript was modified (err=%v)", err)
	}
	if _, err := os.Stat(convPath(f.home, f.dir, "sess-that-never-existed")); !os.IsNotExist(err) {
		t.Fatalf("the reconciler created something at the phantom path (err=%v)", err)
	}
}

// TestReconcileLeavesLiveSessionAlone: the repair must be surgical. A bubble
// whose transcript is present keeps its id, its file, and its silence.
func TestReconcileLeavesLiveSessionAlone(t *testing.T) {
	f := newRepairFixture(t)
	f.k.Reg.RecordSessionID(f.a, "sess-live")
	p := f.plant(t, "sess-live", "{\"type\":\"assistant\"}\n")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	v := f.k.Reg.Version()

	if n := f.run(); n != 0 {
		t.Fatalf("cleared = %d want 0 — a bubble with a real transcript must be left alone", n)
	}
	if sid, _ := f.k.Reg.SessionID(f.a); sid != "sess-live" {
		t.Fatalf("session id = %q want sess-live", sid)
	}
	if got := f.k.Reg.Version(); got != v {
		t.Fatalf("version moved %d -> %d with nothing to repair", v, got)
	}
	after, err := os.ReadFile(p)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the live transcript was modified (err=%v)", err)
	}
	if f.log.String() != "" {
		t.Errorf("a healthy bubble produced a reconcile log line: %q", f.log.String())
	}
}

// TestReconcileClearedIDSurvivesSaveReload: clearing in memory is not the
// repair — the phantom must be gone from fleet.json, or the next start resumes
// it again.
func TestReconcileClearedIDSurvivesSaveReload(t *testing.T) {
	f := newRepairFixture(t)
	base := t.TempDir()
	f.k.Reg.RecordSessionID(f.a, "phantom")

	if n := f.run(); n != 1 {
		t.Fatalf("cleared = %d want 1", n)
	}
	if err := saveFleet(base, f.k, map[int]addr.Address{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	k2 := kernel.New(runner.NewFake())
	restoreFleet(base, k2)
	if sid, _ := k2.Reg.SessionID(f.a); sid != "" {
		t.Fatalf("restored session id = %q want empty — the phantom came back from disk", sid)
	}
}

// TestReconcileSkipsBubblesItCannotJudge: no HOME, no stored id and no dir are
// all "not enough information", and none of them may clear anything.
func TestReconcileSkipsBubblesItCannotJudge(t *testing.T) {
	f := newRepairFixture(t)
	f.k.Reg.RecordSessionID(f.a, "phantom")

	if n := reconcileSessionIDs(f.k, "", f.logf); n != 0 {
		t.Fatalf("cleared = %d with no HOME; a path that cannot be built is not evidence of absence", n)
	}
	if sid, _ := f.k.Reg.SessionID(f.a); sid != "phantom" {
		t.Fatalf("session id = %q want phantom — nothing may be cleared without a resolvable path", sid)
	}

	// A never-launched bubble (no id) is not a repair candidate either.
	f2 := newRepairFixture(t)
	if n := f2.run(); n != 0 {
		t.Fatalf("cleared = %d for a bubble that has never launched", n)
	}
}

// TestUnreadableTranscriptKeepsTheID is the branch a bad day runs through: the
// stat FAILS for a reason that is not "the file is gone" (a permission problem,
// a mount not up yet, a path component that is not a directory). Absence must
// be PROVEN, never inferred from an error, because clearing here destroys a
// working pointer to a live conversation — the exact loss this whole branch
// exists to eliminate, and it would be caused by the repair itself.
//
// ENOTDIR rather than chmod 000: it is deterministic, and it does not quietly
// stop being an error when the suite happens to run as root.
func TestUnreadableTranscriptKeepsTheID(t *testing.T) {
	f := newRepairFixture(t)
	f.k.Reg.RecordSessionID(f.a, "sess-live-but-unreadable")

	// Plant a FILE where the project directory belongs, so any stat below it
	// returns ENOTDIR — an error, and emphatically not IsNotExist.
	projDir := filepath.Dir(convPath(f.home, f.dir, "sess-live-but-unreadable"))
	if err := os.MkdirAll(filepath.Dir(projDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projDir, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(convPath(f.home, f.dir, "sess-live-but-unreadable")); err == nil || os.IsNotExist(err) {
		t.Fatalf("fixture did not produce a non-IsNotExist stat error: %v", err)
	}

	v := f.k.Reg.Version()
	if n := f.run(); n != 0 {
		t.Fatalf("cleared = %d want 0 — a stat that FAILED is not evidence the conversation is gone", n)
	}
	if sid, _ := f.k.Reg.SessionID(f.a); sid != "sess-live-but-unreadable" {
		t.Fatalf("session id = %q want sess-live-but-unreadable — an unreadable path must never destroy a working pointer", sid)
	}
	if got := f.k.Reg.Version(); got != v {
		t.Fatalf("version moved %d -> %d with nothing repaired", v, got)
	}
	// ...and it is never silent: the operator has to be able to see that this
	// bubble was not checked, and why.
	line := f.log.String()
	for _, want := range []string{string(f.a), "sess-live-but-unreadable", "KEPT"} {
		if !strings.Contains(line, want) {
			t.Errorf("an unreadable-path condition must be logged with %q; got %q", want, line)
		}
	}
}

// TestDeliveryAfterRepairCannotResumeAClearedID: clearing the id is only half
// the repair — the point is what the next LAUNCH does. An urgent message wakes
// a cold bubble through EnsureAlive, which launches on whatever id the registry
// holds; after the repair that must be a FRESH conversation, never the phantom.
//
// This is the delivery the startup ordering exists to protect: main binds no
// listener until the repair has run (see TestFleetIsLoadedAndRepairedBefore-
// ListenersBind), so the first delivery a daemon can ever receive sees the
// registry in exactly this state.
func TestDeliveryAfterRepairCannotResumeAClearedID(t *testing.T) {
	f := newRepairFixture(t)
	f.k.Reg.RecordSessionID(f.a, "phantom-conversation")

	if n := f.run(); n != 1 { // startup repair
		t.Fatalf("cleared = %d want 1", n)
	}

	// A delivery arrives (urgent => wake): the path is Send -> deliverMessage ->
	// EnsureAlive.
	if _, err := f.k.Send(addr.Root, f.a, "subj", "body", 0, true); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(f.fr.Launches) != 1 {
		t.Fatalf("launches = %d want 1 — the urgent delivery should have woken the bubble", len(f.fr.Launches))
	}
	got := f.fr.Launches[0]
	if got.Opts.Resume {
		t.Fatalf("the wake RESUMED %q — a cleared id must produce a fresh conversation, not a phantom one", got.Opts.SessionID)
	}
	if got.Opts.SessionID == "" || got.Opts.SessionID == "phantom-conversation" {
		t.Fatalf("launched session id = %q — want a newly minted id", got.Opts.SessionID)
	}
	if sid, _ := f.k.Reg.SessionID(f.a); sid != got.Opts.SessionID {
		t.Fatalf("stored id %q != launched id %q", sid, got.Opts.SessionID)
	}
}

// TestFleetIsLoadedAndRepairedBeforeListenersBind pins the startup ORDERING,
// which is the only thing that makes the repair safe.
//
// startWebhookServer and ipc.Serve both accept traffic the moment they are
// bound, and an urgent delivery reaches EnsureAlive. Bind either of them before
// the fleet is loaded and repaired and there are two ways to lose work: a
// delivery in that window resumes a phantom id, or a bubble launched in that
// window mints a fresh id whose transcript has not been flushed yet and the
// repair clears a LIVE pointer.
//
// Asserted against the source because the property IS the order of statements
// in main, which no runtime seam can observe without becoming a second copy of
// the thing under test. Ordering is used deliberately in preference to a
// "loaded yet?" gate: an unbound listener cannot deliver at all, whereas a gate
// is more state to get wrong on the next delivery path someone adds.
func TestFleetIsLoadedAndRepairedBeforeListenersBind(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	at := func(needle string) int {
		i := strings.Index(string(src), needle)
		if i < 0 {
			t.Fatalf("app.go no longer contains %q — this guard must be updated deliberately, not deleted", needle)
		}
		if j := strings.Index(string(src[i+len(needle):]), needle); j >= 0 {
			t.Fatalf("%q appears more than once in app.go; the guard cannot tell which one binds first", needle)
		}
		return i
	}
	load := at("restoreFleet(baseDir, k)")
	repair := at("reconcileSessionIDs(k, home,")
	for _, bind := range []struct {
		what, needle string
	}{
		{"the webhook server", "startWebhookServer(k)"},
		{"the IPC socket", "ipc.Serve(sock,"},
	} {
		b := at(bind.needle)
		if load > b {
			t.Errorf("restoreFleet runs AFTER %s binds: a delivery in that window launches a bubble the registry has not restored yet", bind.what)
		}
		if repair > b {
			t.Errorf("reconcileSessionIDs runs AFTER %s binds: a delivery in that window can resume a phantom session id, and a launch in that window can have a LIVE id cleared under it", bind.what)
		}
	}
}
