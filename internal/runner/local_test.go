package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

// readUntil reads from f until needle appears or it errors/times out.
func readUntil(f *os.File, needle string) string {
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		tmp := make([]byte, 1024)
		for {
			n, err := f.Read(tmp)
			if n > 0 {
				b.Write(tmp[:n])
				if strings.Contains(b.String(), needle) {
					done <- b.String()
					return
				}
			}
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(3 * time.Second):
		return ""
	}
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

	out := readUntil(ps.PTY(), "go")
	for _, want := range []string{"--dangerously-skip-permissions", "--session-id", "sid-123"} {
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

	out := readUntil(ps.PTY(), "ping-from-test")
	for _, want := range []string{"ARGS:", "--permission-mode", "acceptEdits", "--mcp-config", "find bugs", "ping-from-test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestWrapWithMemCap(t *testing.T) {
	// no cap requested
	if bin, args := wrapWithMemCap("claude", []string{"-p"}, 0, true); bin != "claude" || len(args) != 1 {
		t.Fatalf("memMB=0 should be unchanged, got %s %v", bin, args)
	}
	// cap requested but unsupported -> unchanged
	if bin, _ := wrapWithMemCap("claude", []string{"-p"}, 8192, false); bin != "claude" {
		t.Fatalf("unsupported should be unchanged, got %s", bin)
	}
	// cap requested and supported -> wrapped in systemd-run
	bin, args := wrapWithMemCap("claude", []string{"--foo", "bar"}, 8192, true)
	if bin != "systemd-run" {
		t.Fatalf("bin = %s want systemd-run", bin)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "MemoryMax=8192M") || !strings.Contains(joined, "MemorySwapMax=0") {
		t.Fatalf("missing memory props: %v", args)
	}
	// original command + args must follow the "--" separator, in order
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
