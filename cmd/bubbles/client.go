package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/term"
)

const clientDetachByte = 0x1d // Ctrl-] detaches the client; the fleet keeps running

// runClient attaches the terminal to the workspace daemon (starting it detached
// if it isn't running) and relays bytes both ways. Pressing Ctrl-] detaches and
// leaves the fleet running; quitting from inside (q) stops the whole fleet.
func runClient() {
	baseDir := defaultWorkspace()
	sock := controlSock(baseDir)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		self, _ := os.Executable()
		if serr := startDaemon(self, baseDir); serr != nil {
			fatal(serr)
		}
		conn, err = waitDial(sock, 5*time.Second)
		if err != nil {
			fatal(fmt.Errorf("daemon did not start: %w", err))
		}
	}
	defer conn.Close()

	cols, rows, _ := term.GetSize(int(os.Stdout.Fd()))
	_ = json.NewEncoder(conn).Encode(hello{Rows: uint16(rows), Cols: uint16(cols)})

	var old *term.State
	if st, merr := term.MakeRaw(int(os.Stdin.Fd())); merr == nil {
		old = st
	}
	restore := func() {
		if old != nil {
			_ = term.Restore(int(os.Stdin.Fd()), old)
		}
	}

	done := make(chan string, 2)
	go func() { // app output -> our terminal; ends when the fleet stops
		_, _ = io.Copy(os.Stdout, conn)
		done <- "stopped"
	}()
	go func() { // our keys -> app; Ctrl-] detaches
		buf := make([]byte, 1)
		for {
			n, rerr := os.Stdin.Read(buf)
			if rerr != nil || n == 0 {
				done <- "stopped"
				return
			}
			if buf[0] == clientDetachByte {
				done <- "detached"
				return
			}
			if _, werr := conn.Write(buf[:n]); werr != nil {
				done <- "stopped"
				return
			}
		}
	}()

	reason := <-done
	restore()
	conn.Close()
	if reason == "detached" {
		fmt.Print("\r\n[detached — fleet still running; run `bubbles` to reattach]\r\n")
	} else {
		fmt.Print("\r\n[fleet stopped]\r\n")
	}
}

func waitDial(sock string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", sock); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %s", sock)
}

// startDaemon launches the workspace daemon detached (its own session), logging
// to .bubbles/daemon.log, so it outlives this client.
func startDaemon(self, baseDir string) error {
	logf, _ := os.OpenFile(filepath.Join(baseDir, ".bubbles", "daemon.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(self, "daemon")
	cmd.Dir = baseDir
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
