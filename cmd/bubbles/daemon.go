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

func buildFile(baseDir string) string { return filepath.Join(baseDir, ".bubbles", "daemon.build") }

// buildStamp returns a fingerprint of an executable (its size + modtime), used
// to detect when a newer bubbles has been installed than the running daemon.
func buildStamp(exe string) string {
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}

func writeBuildStamp(baseDir, exe string) {
	if s := buildStamp(exe); s != "" {
		_ = os.WriteFile(buildFile(baseDir), []byte(s), 0o644)
	}
}

// staleDaemon reports whether the installed binary differs from the one the
// running daemon recorded (i.e. an upgrade happened; a restart is needed).
func staleDaemon(baseDir string) bool {
	rec, err := os.ReadFile(buildFile(baseDir))
	if err != nil {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	cur := buildStamp(self)
	return cur != "" && strings.TrimSpace(string(rec)) != cur
}

// controlSock is the daemon's relay socket for a workspace (one daemon per dir).
func controlSock(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "control.sock")
}

// hello is the client's attach handshake (its terminal size). A Control hello
// opens a side channel instead of attaching: the client streams its terminal
// size over it as further hello values whenever its window is resized. Only a
// daemon built from the same binary is sent one (daemonCurrent) — an older
// daemon would take it for a new terminal and drop the real one.
type hello struct {
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
	Control bool   `json:"control,omitempty"`
}

// attachedFile exists while an operator's terminal is attached to the daemon.
// The hosted app reads it (kernel.OperatorAttached) to tell a watched dive from
// one a detached client left behind.
func attachedFile(baseDir string) string {
	return filepath.Join(baseDir, ".bubbles", "client.attached")
}

// operatorAttached reports whether a terminal is attached right now.
func operatorAttached(baseDir string) bool {
	_, err := os.Stat(attachedFile(baseDir))
	return err == nil
}

// daemonCurrent reports whether the running daemon was started from the
// binary installed now — so it understands everything this client speaks.
func daemonCurrent(baseDir string) bool {
	rec, err := os.ReadFile(buildFile(baseDir))
	if err != nil {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	cur := buildStamp(self)
	return cur != "" && strings.TrimSpace(string(rec)) == cur
}

// runDaemon hosts the real bubbles app (`--hosted`) as a child on a PTY and
// relays that PTY to whichever client is attached. The app — kernel, claude
// children, everything — keeps running even with no client attached, so the
// fleet survives the IDE closing.
func runDaemon() {
	baseDir := defaultWorkspace()
	_ = os.MkdirAll(filepath.Join(baseDir, ".bubbles"), 0o755)
	self, _ := os.Executable()
	writeBuildStamp(baseDir, self) // record the binary this daemon is running, for staleness detection
	defer os.Remove(buildFile(baseDir))

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
	_ = os.Remove(attachedFile(baseDir)) // a crashed daemon may have left it
	defer os.Remove(attachedFile(baseDir))

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
		if h.Control {
			// Resize side channel: apply each size the client reports. A
			// SIGWINCH reaches the hosted app, which passes it on to the
			// bubble being viewed.
			go func(c net.Conn, dec *json.Decoder) {
				defer c.Close()
				for {
					var r hello
					if dec.Decode(&r) != nil {
						return
					}
					if r.Rows > 1 && r.Cols > 1 {
						_ = pty.Setsize(ptmx, &pty.Winsize{Rows: r.Rows, Cols: r.Cols})
					}
				}
			}(conn, dec)
			continue
		}

		mu.Lock()
		if client != nil {
			client.Close() // a newer client takes over
		}
		client = conn
		_ = os.WriteFile(attachedFile(baseDir), nil, 0o644)
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
				_ = os.Remove(attachedFile(baseDir))
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
