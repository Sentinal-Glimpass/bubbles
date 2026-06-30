package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

func pidFile(baseDir string) string { return filepath.Join(baseDir, ".bubbles", "daemon.pid") }

// controlSock is the daemon's relay socket for a workspace (one daemon per dir).
func controlSock(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "control.sock")
}

// hello is the client's attach handshake (its terminal size).
type hello struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// runDaemon hosts the real bubbles app (`--hosted`) as a child on a PTY and
// relays that PTY to whichever client is attached. The app — kernel, claude
// children, everything — keeps running even with no client attached, so the
// fleet survives the IDE closing.
func runDaemon() {
	baseDir := defaultWorkspace()
	_ = os.MkdirAll(filepath.Join(baseDir, ".bubbles"), 0o755)
	self, _ := os.Executable()

	cmd := exec.Command(self, "--hosted")
	cmd.Dir = baseDir
	ptmx, err := pty.Start(cmd)
	if err != nil {
		fatal(err)
	}
	defer ptmx.Close()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120}) // default until a client attaches

	sock := controlSock(baseDir)
	_ = os.Remove(sock) // clear a stale socket
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)

	_ = os.WriteFile(pidFile(baseDir), []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidFile(baseDir))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sigCh; _ = cmd.Process.Kill(); ln.Close() }() // `bubbles stop` / signal -> tear down

	var mu sync.Mutex
	var client net.Conn

	// PTY output -> the attached client (discarded when none, so the app never blocks).
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				c := client
				mu.Unlock()
				if c != nil {
					_, _ = c.Write(buf[:n])
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	go func() { _ = cmd.Wait(); ln.Close() }() // app quit -> stop accepting -> daemon exits

	for {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		dec := json.NewDecoder(conn)
		var h hello
		_ = dec.Decode(&h)

		mu.Lock()
		if client != nil {
			client.Close() // a newer client takes over
		}
		client = conn
		mu.Unlock()

		if h.Rows > 1 && h.Cols > 1 { // apply size + nudge a repaint
			ws := &pty.Winsize{Rows: h.Rows, Cols: h.Cols}
			small := *ws
			small.Rows--
			_ = pty.Setsize(ptmx, &small)
			time.Sleep(50 * time.Millisecond)
			_ = pty.Setsize(ptmx, ws)
		}

		go func(c net.Conn, buffered io.Reader) { // client input -> PTY
			_, _ = io.Copy(ptmx, io.MultiReader(buffered, c))
			mu.Lock()
			if client == c {
				client = nil
			}
			mu.Unlock()
		}(conn, dec.Buffered())
	}
}

// runStop stops the workspace's persistent fleet (kills the daemon and, via the
// closed PTY, its app + claude children).
func runStop() {
	baseDir := defaultWorkspace()
	data, err := os.ReadFile(pidFile(baseDir))
	if err != nil {
		fmt.Println("no fleet running in this directory")
		return
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 0 {
		fmt.Println("no fleet running in this directory")
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM) // the daemon's whole session group
	_ = syscall.Kill(pid, syscall.SIGTERM)
	fmt.Println("fleet stopped")
}
