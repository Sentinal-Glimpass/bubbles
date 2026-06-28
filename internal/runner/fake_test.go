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
