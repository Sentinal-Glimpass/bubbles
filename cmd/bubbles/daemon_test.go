package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDaemonRelay builds the binary, starts the daemon (which hosts the real app
// on a PTY), then attaches as a client and confirms the app's TUI is relayed,
// the daemon survives a detach, and a fresh client can reattach. No claude needed
// (an empty fleet just renders the title).
func TestDaemonRelay(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bubbles")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	d := exec.Command(bin, "daemon")
	d.Dir = dir
	d.Env = append(os.Environ(), "TERM=xterm-256color")
	d.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-d.Process.Pid, syscall.SIGKILL) // kill the whole group (daemon + app)
		_ = d.Wait()
	}()

	sock := filepath.Join(dir, ".bubbles", "control.sock")

	conn := dialWithin(t, sock, 5*time.Second)
	sendHello(conn)
	if !readContains(conn, "BUBBLES", 6*time.Second) {
		t.Fatal("relay did not carry the app's TUI output")
	}
	conn.Close() // detach

	// daemon must still be alive: a new client reattaches and sees the TUI again
	conn2 := dialWithin(t, sock, 3*time.Second)
	defer conn2.Close()
	sendHello(conn2)
	if !readContains(conn2, "BUBBLES", 6*time.Second) {
		t.Fatal("reattach failed — daemon should survive a client detach")
	}

	// keystrokes must flow client -> daemon -> app: 'n' opens the spawn prompt
	if _, err := conn2.Write([]byte("n")); err != nil {
		t.Fatalf("write keystroke: %v", err)
	}
	if !readContains(conn2, "persona", 6*time.Second) {
		t.Fatal("keystroke relay failed — app did not react to 'n'")
	}

	// q in the fleet view must DETACH (emit the sentinel), not kill the fleet.
	// Pace the keys so the input parser doesn't fuse Esc+q into Alt+q.
	_, _ = conn2.Write([]byte{0x1b}) // esc: leave the spawn prompt
	time.Sleep(400 * time.Millisecond)
	_, _ = conn2.Write([]byte("q"))
	if !readContains(conn2, detachSentinel, 6*time.Second) {
		t.Fatal("q should emit the detach sentinel (fleet keeps running)")
	}
	conn2.Close()

	// fleet survives q: a fresh client still reattaches
	conn3 := dialWithin(t, sock, 3*time.Second)
	defer conn3.Close()
	sendHello(conn3)
	if !readContains(conn3, "BUBBLES", 6*time.Second) {
		t.Fatal("fleet should survive q (detach) — reattach failed")
	}
}

func TestSentinelScanner(t *testing.T) {
	var out bytes.Buffer
	s := &sentinelScanner{w: &out, sentinel: []byte("XYZ")}
	if s.write([]byte("helloX")) {
		t.Fatal("no full sentinel yet")
	}
	if !s.write([]byte("YZworld")) { // sentinel straddles the boundary
		t.Fatal("should detect sentinel across writes")
	}
	if out.String() != "hello" {
		t.Fatalf("output = %q want %q", out.String(), "hello")
	}
}

// TestDaemonStop confirms `bubbles stop` tears the daemon down.
func TestDaemonStop(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bubbles")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	d := exec.Command(bin, "daemon")
	d.Dir = dir
	d.Env = append(os.Environ(), "TERM=xterm-256color")
	d.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer func() { _ = syscall.Kill(-d.Process.Pid, syscall.SIGKILL) }() // backstop

	sock := filepath.Join(dir, ".bubbles", "control.sock")
	dialWithin(t, sock, 5*time.Second).Close() // wait until up

	stop := exec.Command(bin, "stop")
	stop.Dir = dir
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("stop: %v (%s)", err, out)
	}

	waited := make(chan error, 1)
	go func() { waited <- d.Wait() }()
	select {
	case <-waited: // daemon exited — good
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after `bubbles stop`")
	}
}

func dialWithin(t *testing.T, sock string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("could not connect to daemon at %s", sock)
	return nil
}

func sendHello(conn net.Conn) {
	_ = json.NewEncoder(conn).Encode(map[string]uint16{"rows": 40, "cols": 120})
}

func readContains(conn net.Conn, needle string, timeout time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), needle) {
				return true
			}
		}
		if err != nil {
			return strings.Contains(sb.String(), needle)
		}
	}
}
