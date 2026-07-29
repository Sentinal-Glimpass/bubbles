package runner

import (
	"errors"
	"testing"
)

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

func TestFakeRunnerLaunchSucceedsByDefault(t *testing.T) {
	r := NewFake()
	// FailLaunch is additive: the ~100 tests that never touch it must keep
	// seeing a nil error and a live session.
	sess, err := r.Launch("0.1", "/tmp/x", SpawnOpts{})
	if err != nil {
		t.Fatalf("default Launch returned %v, want nil", err)
	}
	if sess == nil || !sess.Alive() {
		t.Fatal("default Launch must yield a live session")
	}
}

func TestFakeRunnerFailLaunch(t *testing.T) {
	r := NewFake()
	r.FailLaunch = true
	sess, err := r.Launch("0.1", "/nonexistent", SpawnOpts{})
	if !errors.Is(err, ErrFakeLaunch) {
		t.Fatalf("err = %v want ErrFakeLaunch", err)
	}
	if sess != nil {
		t.Fatalf("failed Launch must yield no session, got %v", sess)
	}
	// The attempt is still recorded, so a test can tell a failed launch from a
	// suppressed one.
	if len(r.Launches) != 1 {
		t.Fatalf("Launches = %d want 1", len(r.Launches))
	}
	if r.Session("0.1") != nil {
		t.Fatal("failed Launch must not enter the session table")
	}
}
