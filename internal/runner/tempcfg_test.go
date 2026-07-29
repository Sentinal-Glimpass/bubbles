package runner

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// touch creates an empty file, failing the test if it cannot.
func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// names lists the directory's entries, sorted, for stable assertions.
func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestSweepRemovesOwnOrphansOnly is the safety property the whole sweep hangs
// on. Another bubbles daemon may be running on this host with live sessions;
// deleting its configs is a self-inflicted outage in a fleet we do not own.
// Only files carrying OUR pid may ever be removed.
func TestSweepRemovesOwnOrphansOnly(t *testing.T) {
	dir := t.TempDir()
	const mine = 4242

	touch(t, dir, "bubbles-mcp-4242-0.1.json")      // ours, orphaned
	touch(t, dir, "bubbles-settings-4242-0.1.json") // ours, orphaned
	touch(t, dir, "bubbles-mcp-4242-0.2.json")      // ours, orphaned

	touch(t, dir, "bubbles-mcp-9999-0.1.json")      // ANOTHER live daemon
	touch(t, dir, "bubbles-settings-9999-0.1.json") // ANOTHER live daemon
	// A pid whose decimal form starts with ours: the match must not be a bare
	// numeric prefix.
	touch(t, dir, "bubbles-mcp-42425-0.1.json")
	touch(t, dir, "bubbles-mcp-424-0.1.json")
	// Unrelated names that merely start with "bubbles-".
	touch(t, dir, "bubbles-4242.json")
	touch(t, dir, "bubbles-mcp-4242-0.1.json.bak")
	touch(t, dir, "somebody-elses-file")

	if n := sweepTempConfigs(dir, mine, nil); n != 3 {
		t.Fatalf("removed %d files, want 3", n)
	}
	want := []string{
		"bubbles-4242.json",
		"bubbles-mcp-424-0.1.json",
		"bubbles-mcp-4242-0.1.json.bak",
		"bubbles-mcp-42425-0.1.json",
		"bubbles-mcp-9999-0.1.json",
		"bubbles-settings-9999-0.1.json",
		"somebody-elses-file",
	}
	got := names(t, dir)
	if len(got) != len(want) {
		t.Fatalf("survivors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("survivors = %v, want %v", got, want)
		}
	}
}

// TestSweepSpareLiveSessions: a file belonging to an address that still has a
// session must survive even though it carries our pid.
func TestSweepSparesLiveSessions(t *testing.T) {
	dir := t.TempDir()
	const mine = 77
	touch(t, dir, "bubbles-mcp-77-0.1.json")
	touch(t, dir, "bubbles-settings-77-0.1.json")
	touch(t, dir, "bubbles-mcp-77-0.2.json")

	keep := map[string]bool{"bubbles-mcp-77-0.1.json": true, "bubbles-settings-77-0.1.json": true}
	if n := sweepTempConfigs(dir, mine, keep); n != 1 {
		t.Fatalf("removed %d files, want 1", n)
	}
	if got := names(t, dir); len(got) != 2 {
		t.Fatalf("survivors = %v, want the two live-session files", got)
	}
}

// TestSweepIsIdempotentAndSafeWhenMissing covers the periodic caller: it runs
// forever, often with nothing to do, sometimes before the directory exists.
func TestSweepIsIdempotentAndSafeWhenMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no", "such", "dir")
	for i := 0; i < 3; i++ {
		if n := sweepTempConfigs(missing, 1, nil); n != 0 {
			t.Fatalf("sweep of a missing dir removed %d files", n)
		}
	}

	dir := t.TempDir()
	touch(t, dir, "bubbles-mcp-5-0.1.json")
	if n := sweepTempConfigs(dir, 5, nil); n != 1 {
		t.Fatalf("first sweep removed %d, want 1", n)
	}
	for i := 0; i < 3; i++ {
		if n := sweepTempConfigs(dir, 5, nil); n != 0 {
			t.Fatalf("repeat sweep removed %d files, want 0", n)
		}
	}
}

// TestTempConfigPathsRoundTrip pins that the names the sweep matches are
// exactly the names the launch path writes — the two must never drift apart,
// or the sweep silently stops reaping anything.
func TestTempConfigPathsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := addr.Address("0.3.12")
	for _, p := range []string{mcpConfigPath(dir, 4242, a), settingsFilePath(dir, 4242, a)} {
		if !ownsTempConfig(filepath.Base(p), 4242) {
			t.Errorf("%s is not recognised as owned by pid 4242", filepath.Base(p))
		}
		if ownsTempConfig(filepath.Base(p), 4243) {
			t.Errorf("%s was claimed by the wrong pid", filepath.Base(p))
		}
	}
}

// TestKillRemovesTempConfigs proves the primary (non-sweep) cleanup path: a
// session's configs go away when the session is killed, so the sweep is a
// backstop rather than the mechanism.
func TestKillRemovesTempConfigs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if os.TempDir() != dir {
		t.Skipf("os.TempDir() not redirectable on this platform (got %s)", os.TempDir())
	}
	work := t.TempDir()
	stub := filepath.Join(work, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := addr.Address("0.1")
	pid := os.Getpid()

	r := NewLocal()
	r.Bin = stub
	r.InterruptByte = 0
	r.MCPConfig = func(addr.Address) string { return "{}" }
	r.SessionFile = func(addr.Address) string { return filepath.Join(work, "session.id") }
	if _, err := r.Launch(a, work, SpawnOpts{Persona: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{mcpConfigPath(dir, pid, a), settingsFilePath(dir, pid, a)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("launch did not write %s: %v", filepath.Base(p), err)
		}
	}
	if err := r.Kill(a); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := names(t, dir); len(got) != 0 {
		t.Errorf("Kill left temp configs behind: %v", got)
	}
}

// launchStub builds a runner whose "claude" is a stub that just sits reading
// stdin, with the temp dir redirected so the test sees exactly the files this
// launch writes.
func launchStub(t *testing.T) (r *LocalRunner, tmp, work string) {
	t.Helper()
	tmp = t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if os.TempDir() != tmp {
		t.Skipf("os.TempDir() not redirectable on this platform (got %s)", os.TempDir())
	}
	work = t.TempDir()
	stub := filepath.Join(work, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r = NewLocal()
	r.Bin = stub
	r.MCPConfig = func(addr.Address) string { return "{}" }
	r.SessionFile = func(addr.Address) string { return filepath.Join(work, "session.id") }
	return r, tmp, work
}

// wantTempConfigs fails unless both of a's temp configs are present.
func wantTempConfigs(t *testing.T, tmp string, a addr.Address, why string) {
	t.Helper()
	for _, p := range []string{mcpConfigPath(tmp, os.Getpid(), a), settingsFilePath(tmp, os.Getpid(), a)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %s is gone: %v", why, filepath.Base(p), err)
		}
	}
}

// A Kill of an address whose relaunch is already in flight must not delete the
// relaunch's config files. Dropping the session from the map is precisely what
// frees a concurrent ensureAlive to relaunch, so a removal done after the lock
// is released targets the NEW files — and the bubble boots with no MCP server.
// The interleaving is forced with the CanSpawn hook, which Launch calls after
// it has written both configs and before it registers the session.
func TestKillDuringRelaunchSparesTheNewConfigs(t *testing.T) {
	r, tmp, work := launchStub(t)
	a := addr.Address("0.1")

	// The session an EvictIdle/budget sweep would be killing.
	if _, err := r.Launch(a, work, SpawnOpts{Persona: "x"}); err != nil {
		t.Fatal(err)
	}

	killed := false
	r.CitizenPrompt = "citizen"
	r.CanSpawn = func(addr.Address) bool {
		if !killed {
			killed = true
			_ = r.Kill(a) // the concurrent kill, landing mid-relaunch
		}
		return false
	}
	if _, err := r.Launch(a, work, SpawnOpts{Persona: "x"}); err != nil {
		t.Fatal(err)
	}
	if !killed {
		t.Fatal("the interleaving hook never ran")
	}
	wantTempConfigs(t, tmp, a, "Kill raced the relaunch")
	_ = r.Kill(a)
}

// The periodic sweep snapshots the live set from r.sessions, but a launch that
// has written its configs has not registered there yet. Without the in-flight
// claim the address is absent from keep and the sweep deletes a live launch's
// config.
func TestSweepDuringLaunchSparesTheNewConfigs(t *testing.T) {
	r, tmp, work := launchStub(t)
	a := addr.Address("0.2")

	swept := false
	r.CitizenPrompt = "citizen"
	r.CanSpawn = func(addr.Address) bool {
		if !swept {
			swept = true
			r.SweepTempConfigs() // the 10-minute sweep, landing mid-launch
		}
		return false
	}
	if _, err := r.Launch(a, work, SpawnOpts{Persona: "x"}); err != nil {
		t.Fatal(err)
	}
	if !swept {
		t.Fatal("the interleaving hook never ran")
	}
	wantTempConfigs(t, tmp, a, "the sweep ran mid-launch")
	_ = r.Kill(a)
}
