package runner

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// syncBuf is a concurrency-safe buffer the drainer can stream into from its
// goroutine while a test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// attachUntil subscribes to a session's output (replaying scrollback) and waits
// until needle appears or it times out. This is how a viewer sees output now —
// the PTY is drained by the session itself, not read directly.
func attachUntil(ps PTYSession, needle string) string {
	var buf syncBuf
	detach := ps.Attach(&buf)
	defer detach()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), needle) {
			return buf.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return buf.String()
}

func TestLocalRunnerFlags(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ARGS:$@\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	allow := true
	r := NewLocal()
	r.Bin = stub
	r.InterruptByte = 0
	r.AllowAll = &allow

	sess, err := r.Launch("0.1", dir, SpawnOpts{Persona: "x", SessionID: "sid-123"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill("0.1")
	ps := sess.(PTYSession)
	time.Sleep(150 * time.Millisecond)
	sess.Write([]byte("go"))

	out := attachUntil(ps, "go")
	for _, want := range []string{"--dangerously-skip-permissions", "--session-id", "sid-123", "--disallowed-tools", "AskUserQuestion"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--permission-mode") {
		t.Fatalf("allow-all should not set --permission-mode:\n%s", out)
	}
}

func TestLocalRunnerLaunchAndDeliver(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ARGS:$@\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewLocal()
	r.Bin = stub
	r.InterruptByte = 0 // keep stub output clean for assertions
	r.CitizenPrompt = "be a good citizen"
	r.MCPConfig = func(a addr.Address) string { return `{"mcpServers":{}}` }

	sess, err := r.Launch("0.1", dir, SpawnOpts{Persona: "scout", Goal: "find bugs"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer r.Kill("0.1")

	ps, ok := sess.(PTYSession)
	if !ok {
		t.Fatal("session is not a PTYSession")
	}

	time.Sleep(150 * time.Millisecond) // let the stub start and begin reading stdin
	if _, err := sess.Write([]byte("ping-from-test")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := attachUntil(ps, "ping-from-test")
	for _, want := range []string{"ARGS:", "--permission-mode", "acceptEdits", "--mcp-config", "--strict-mcp-config", "find bugs", "ping-from-test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

// TestPTYDrainerNeverStalls is the core regression test for the "sessions stall
// while working / balloon to 8GB" bug: with NOBODY attached, a session must
// still be drained, so writing far more than the kernel PTY buffer (~64KB) never
// blocks the process. Before the persistent drainer, this write would deadlock.
func TestPTYDrainerNeverStalls(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	s := &ptySession{ptmx: ptmx}
	go s.drain()

	big := bytes.Repeat([]byte("x"), 512*1024) // >> any PTY buffer, no viewer attached
	done := make(chan error, 1)
	go func() {
		_, werr := tty.Write(big)
		done <- werr
	}()
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("write to PTY: %v", werr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write to an unviewed PTY stalled — the drainer is not reading it")
	}

	// scrollback stays bounded even under a flood
	s.rmu.Lock()
	rl := len(s.ring)
	s.rmu.Unlock()
	if rl > scrollbackCap {
		t.Fatalf("scrollback ring exceeded cap: %d > %d", rl, scrollbackCap)
	}
}

// TestPTYAttachReplaysThenStreams: attaching replays what was produced before
// the viewer arrived (so dive-in isn't a black screen), then streams live output.
func TestPTYAttachReplaysThenStreams(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	s := &ptySession{ptmx: ptmx}
	go s.drain()

	if _, err := tty.Write([]byte("BEFORE-attach\n")); err != nil {
		t.Fatal(err)
	}
	// wait for the drainer to capture it into scrollback
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.rmu.Lock()
		got := strings.Contains(string(s.ring), "BEFORE-attach")
		s.rmu.Unlock()
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var buf syncBuf
	detach := s.Attach(&buf)
	defer detach()
	if !strings.Contains(buf.String(), "BEFORE-attach") {
		t.Fatalf("attach should replay scrollback, got %q", buf.String())
	}

	if _, err := tty.Write([]byte("AFTER-attach\n")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "AFTER-attach") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "AFTER-attach") {
		t.Fatalf("attach should stream live output, got %q", buf.String())
	}

	// after detach, live output stops flowing to the viewer
	detach()
	before := buf.String()
	if _, err := tty.Write([]byte("POST-detach\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if buf.String() != before {
		t.Fatalf("detached viewer should receive nothing more, got %q", buf.String())
	}
}

// aliveCmd starts a long-lived process so a test ptySession's Alive() probe
// returns true, independent of the pty.Open pair used for I/O.
func aliveCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill() })
	return c
}

// TestDefaultModelSkip: an empty DefaultModel (and no per-bubble model) passes
// NO --model, so claude uses its own default (ANTHROPIC_MODEL / Bedrock). A set
// default is passed through.
func TestDefaultModelSkip(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ARGS:$@\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTHROPIC_MODEL", "") // isolate from the dev shell's env

	// DefaultModel "" -> no --model
	r := NewLocal()
	r.Bin = stub
	r.DefaultModel = ""
	sess, err := r.Launch("0.1", dir, SpawnOpts{Persona: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Kill("0.1")
	if out := attachUntil(sess.(PTYSession), "ARGS:"); strings.Contains(out, "--model") {
		t.Fatalf("empty DefaultModel should pass no --model:\n%s", out)
	}

	// DefaultModel set to a bedrock-style id -> passed through
	r2 := NewLocal()
	r2.Bin = stub
	r2.DefaultModel = "us.anthropic.claude-sonnet-4-v1:0"
	s2, err := r2.Launch("0.2", dir, SpawnOpts{Persona: "y"})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Kill("0.2")
	if out := attachUntil(s2.(PTYSession), "ARGS:"); !strings.Contains(out, "--model us.anthropic.claude-sonnet-4-v1:0") {
		t.Fatalf("set DefaultModel should be passed:\n%s", out)
	}

	// ANTHROPIC_MODEL set -> no --model at all, even with a per-bubble model
	// (a --model flag would override the env var; env is the source of truth)
	t.Setenv("ANTHROPIC_MODEL", "us.anthropic.claude-opus-4-6-v1[1m]")
	r3 := NewLocal()
	r3.Bin = stub
	s3, err := r3.Launch("0.3", dir, SpawnOpts{Persona: "z", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Kill("0.3")
	if out := attachUntil(s3.(PTYSession), "ARGS:"); strings.Contains(out, "--model") {
		t.Fatalf("ANTHROPIC_MODEL set should suppress --model entirely:\n%s", out)
	}
}

func TestParseTokenCount(t *testing.T) {
	cases := map[string]int{
		"This session is 512,340 tokens long": 512340,
		"1.2M tokens":                          1_200_000,
		"620k tokens":                          620_000,
		"no number here":                       0,
		"version 2.1.199 released":             0, // not near "tokens" -> ignored
	}
	for in, want := range cases {
		if got := parseTokenCount(in); got != want {
			t.Errorf("parseTokenCount(%q) = %d want %d", in, got, want)
		}
	}
}

// TestResumeMenuAutopilot: the watcher detects the resume menu and picks the
// option per the token threshold — Enter for summary (>= threshold), Down+Enter
// for as-is (< threshold) — then marks the session input-ready.
func TestResumeMenuAutopilot(t *testing.T) {
	run := func(t *testing.T, tokens int, wantArrow bool) {
		ptmx, tty, err := pty.Open()
		if err != nil {
			t.Skipf("pty.Open unavailable: %v", err)
		}
		defer ptmx.Close()
		defer tty.Close()
		if old, err := term.MakeRaw(int(tty.Fd())); err == nil { // no CR/NL translation
			defer term.Restore(int(tty.Fd()), old)
		}

		s := &ptySession{ptmx: ptmx, cmd: aliveCmd(t), resume: true, menuThreshold: 500_000, created: time.Now()}
		go s.drain()
		go s.readyWatcher()

		// claude "loads" then renders the menu
		fmt.Fprintf(tty, "loading conversation (%d tokens)...\r\n", tokens)
		time.Sleep(120 * time.Millisecond)
		fmt.Fprintf(tty, "This session is %d tokens.\r\n  %s (recommended)\r\n  %s\r\n", tokens, resumeMenuOpt1, resumeMenuOpt2)

		// capture what the autopilot types back
		got := make(chan []byte, 1)
		go func() {
			buf := make([]byte, 64)
			n, _ := tty.Read(buf)
			got <- buf[:n]
		}()
		var keys []byte
		select {
		case keys = <-got:
		case <-time.After(3 * time.Second):
			t.Fatal("autopilot did not answer the menu")
		}
		hasArrow := bytes.Contains(keys, []byte{0x1b, '[', 'B'})
		if hasArrow != wantArrow {
			t.Fatalf("tokens=%d: down-arrow=%v want %v (keys=%q)", tokens, hasArrow, wantArrow, keys)
		}
		deadline := time.Now().Add(2 * time.Second)
		for !s.InputReady() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !s.InputReady() {
			t.Fatal("session should be input-ready after answering the menu")
		}
	}
	t.Run("big->summary", func(t *testing.T) { run(t, 800_000, false) }) // Enter only
	t.Run("small->as-is", func(t *testing.T) { run(t, 120_000, true) })  // Down + Enter
}

// TestFreshLaunchBecomesReady: a non-resume session is input-ready shortly after
// it produces output (no menu to wait for).
func TestFreshLaunchBecomesReady(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open unavailable: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	s := &ptySession{ptmx: ptmx, cmd: aliveCmd(t), resume: false, menuThreshold: 500_000, created: time.Now()}
	go s.drain()
	go s.readyWatcher()
	if s.InputReady() {
		t.Fatal("should not be ready before any output")
	}
	fmt.Fprint(tty, "claude UI\r\n")
	deadline := time.Now().Add(3 * time.Second)
	for !s.InputReady() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !s.InputReady() {
		t.Fatal("a fresh session should become ready after producing output")
	}
}

func TestWrapInScope(t *testing.T) {
	// unsupported -> unchanged (macOS / no systemd)
	if bin, _ := wrapInScope("claude", []string{"-p"}, 0, false); bin != "claude" {
		t.Fatalf("unsupported should be unchanged, got %s", bin)
	}
	// supported, NO cap -> still scoped for measurement, but WITHOUT MemoryMax
	bin, args := wrapInScope("claude", []string{"-p"}, 0, true)
	if bin != "systemd-run" {
		t.Fatalf("uncapped should still be scoped, got %s", bin)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--scope") || strings.Contains(joined, "MemoryMax") {
		t.Fatalf("uncapped scope should have no MemoryMax: %v", args)
	}
	// supported, WITH cap -> scoped + MemoryMax
	_, args = wrapInScope("claude", []string{"--foo", "bar"}, 8192, true)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "MemoryMax=8192M") || !strings.Contains(joined, "MemorySwapMax=0") {
		t.Fatalf("capped scope missing props: %v", args)
	}
	// original command + args follow "--" in order
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || args[sep+1] != "claude" || args[sep+2] != "--foo" || args[sep+3] != "bar" {
		t.Fatalf("command not preserved after --: %v", args)
	}
}
