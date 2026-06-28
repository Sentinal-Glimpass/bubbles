package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
)

// TestMCPHelperBinaryEndToEnd builds the real binary and drives its mcp-stdio
// mode against a live IPC socket, proving the MCP bridge end to end (no claude).
func TestMCPHelperBinaryEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bubbles")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	sock := filepath.Join(dir, "b.sock")
	var mu sync.Mutex
	var got []ipc.Request
	ln, err := ipc.Serve(sock, func(r ipc.Request) ipc.Reply {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if r.Op == "contacts" {
			return ipc.Reply{OK: true, Contacts: []string{"0"}}
		}
		return ipc.Reply{OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cmd := exec.Command(bin, "mcp-stdio")
	cmd.Env = append(os.Environ(), "BUBBLE_ADDR=0.1", "BUBBLE_SOCK="+sock, "BUBBLE_SPAWNABLE=0")
	stdin, _ := cmd.StdinPipe()
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	io.WriteString(stdin, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send","arguments":{"to":"0","subject":"hi"}}}`,
	}, "\n")+"\n")
	stdin.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("mcp-stdio helper timed out")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Op != "send" || got[0].From != "0.1" || got[0].To != "0" {
		t.Fatalf("relayed requests = %+v, want one send from 0.1 to 0", got)
	}
	if !strings.Contains(out.String(), "sent to 0") {
		t.Fatalf("helper stdout missing tool result:\n%s", out.String())
	}
}
