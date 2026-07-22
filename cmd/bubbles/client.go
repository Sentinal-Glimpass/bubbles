package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	clientLeaderByte = 0x1c // Ctrl-\ : arms the stop chord (also the in-bubble leader)
	clientLeaderAlt  = 0x1f // Ctrl-/ : an interchangeable leader (sends US, 0x1f)
	clientStopByte   = 0x1d // Ctrl-] : completes <leader> Ctrl-] to stop the fleet
)

func isClientLeader(b byte) bool { return b == clientLeaderByte || b == clientLeaderAlt }

// chordStep advances the fleet-stop chord for one input byte. armedWith is 0
// when not armed, else the leader byte that armed it. A lone leader (Ctrl-\ or
// Ctrl-/) arms; leader then Ctrl-] stops the fleet; leader then ANY other key
// forwards BOTH bytes (so the in-bubble leader — <leader><leader> to pop to the
// fleet, <leader> digit to jump — still works, and with the actual leader the
// user pressed). A bare Ctrl-] no longer stops anything, so it can't tear down
// the fleet by accident.
func chordStep(armedWith, b byte) (forward []byte, stop bool, nowArmedWith byte) {
	if armedWith != 0 {
		if b == clientStopByte {
			return nil, true, 0
		}
		return []byte{armedWith, b}, false, 0
	}
	if isClientLeader(b) {
		return nil, false, b
	}
	return []byte{b}, false, 0
}

// runClient attaches the terminal to the workspace daemon (starting it detached
// if it isn't running) and relays bytes both ways. Quitting from inside (q)
// detaches (fleet keeps running); a leader (Ctrl-\ or Ctrl-/) then Ctrl-] stops
// the whole fleet.
func runClient() {
	baseDir := defaultWorkspace()
	sock := controlSock(baseDir)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		self, _ := os.Executable()
		// No daemon for this workspace yet — we're about to start one, which binds
		// the webhook port. Resolve a port collision interactively HERE (foreground,
		// before raw mode) so issued webhook URLs stay on a known, fixed port.
		ensureWebhookPortFree()
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
	go func() { // app output -> our terminal; detach when the app emits the sentinel
		sc := &sentinelScanner{w: os.Stdout, sentinel: []byte(detachSentinel)}
		buf := make([]byte, 4096)
		for {
			n, rerr := conn.Read(buf)
			if n > 0 && sc.write(buf[:n]) {
				done <- "detached"
				return
			}
			if rerr != nil {
				done <- "stopped"
				return
			}
		}
	}()
	go func() { // our keys -> app; <leader> Ctrl-] stops the fleet (a bare Ctrl-] does not)
		var armedWith byte
		buf := make([]byte, 1)
		for {
			n, rerr := os.Stdin.Read(buf)
			if rerr != nil || n == 0 {
				done <- "stopped"
				return
			}
			var forward []byte
			var stop bool
			forward, stop, armedWith = chordStep(armedWith, buf[0])
			if stop {
				done <- "stop"
				return
			}
			if len(forward) > 0 {
				if _, werr := conn.Write(forward); werr != nil {
					done <- "stopped"
					return
				}
			}
		}
	}()

	reason := <-done
	restore()
	conn.Close()
	switch reason {
	case "detached":
		fmt.Print("\r\n[detached — fleet still running; run `bubbles` to reattach]\r\n")
	case "stop":
		runStop() // tear down the daemon + fleet
		fmt.Print("\r\n[fleet stopped]\r\n")
	default:
		fmt.Print("\r\n[fleet stopped]\r\n")
	}
}

// sentinelScanner writes a byte stream to w but strips (and detects) a sentinel
// that may straddle write boundaries.
type sentinelScanner struct {
	w        io.Writer
	sentinel []byte
	carry    []byte
}

func (s *sentinelScanner) write(p []byte) bool {
	data := make([]byte, 0, len(s.carry)+len(p))
	data = append(data, s.carry...)
	data = append(data, p...)
	if i := bytes.Index(data, s.sentinel); i >= 0 {
		_, _ = s.w.Write(data[:i])
		s.carry = nil
		return true
	}
	keep := len(s.sentinel) - 1 // a partial sentinel might continue next time
	if keep > len(data) {
		keep = len(data)
	}
	_, _ = s.w.Write(data[:len(data)-keep])
	s.carry = append([]byte(nil), data[len(data)-keep:]...)
	return false
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

// livezInfo is the /livez response identifying a bubbles webhook server.
type livezInfo struct {
	Bubbles   bool   `json:"bubbles"`
	PID       int    `json:"pid"`
	Workspace string `json:"workspace"`
}

// ensureWebhookPortFree makes the bubbles webhook port available before the
// daemon starts. If the port is held by ANOTHER bubbles fleet, it offers to kill
// that fleet and take over; otherwise (or if declined) it asks for a different
// port. Runs in the foreground (line-buffered stdin) before the TUI's raw mode.
func ensureWebhookPortFree() {
	reader := bufio.NewReader(os.Stdin)
	for {
		port := webhookPort()
		if portFree(port) {
			return // free — the daemon will bind it
		}
		if info := probeBubblesLivez(port); info != nil {
			fmt.Printf("\nbubbles: port %d is already in use by another bubbles fleet:\n  workspace: %s\n  pid: %d\n", port, info.Workspace, info.PID)
			fmt.Print("Kill that fleet and take over the port? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.EqualFold(strings.TrimSpace(line), "y") {
				if killFleet(info.Workspace, info.PID) && waitPortFree(port, 6*time.Second) {
					fmt.Printf("bubbles: took over port %d.\n", port)
					return
				}
				fmt.Printf("bubbles: could not free port %d.\n", port)
			}
		} else {
			fmt.Printf("\nbubbles: port %d is in use by another (non-bubbles) process.\n", port)
		}
		fmt.Print("Enter a different port for bubbles (blank to abort): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			fatal(fmt.Errorf("port %d unavailable and no alternative given", port))
		}
		n, cerr := strconv.Atoi(line)
		if cerr != nil || n <= 0 || n >= 65536 {
			fmt.Println("Invalid port number.")
			continue
		}
		_ = os.Setenv("BUBBLES_WEBHOOK_PORT", strconv.Itoa(n)) // the daemon inherits this
	}
}

// portFree reports whether a TCP port on loopback can be bound right now.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// probeBubblesLivez returns the /livez identity if a BUBBLES server holds the
// port, else nil (foreign process or nothing there).
func probeBubblesLivez(port int) *livezInfo {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/livez", port))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info livezInfo
	if json.NewDecoder(resp.Body).Decode(&info) != nil || !info.Bubbles {
		return nil
	}
	return &info
}

// killFleet stops another bubbles fleet holding the port. It prefers the
// workspace's daemon.pid (the session leader — a clean whole-fleet shutdown);
// the /livez app pid is a fallback.
func killFleet(workspace string, appPID int) bool {
	if workspace != "" {
		if data, err := os.ReadFile(filepath.Join(workspace, ".bubbles", "daemon.pid")); err == nil {
			if dpid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && dpid > 0 {
				_ = syscall.Kill(dpid, syscall.SIGTERM)
				return true
			}
		}
	}
	if appPID > 0 {
		_ = syscall.Kill(appPID, syscall.SIGTERM)
		return true
	}
	return false
}

// waitPortFree polls until the port frees or the timeout elapses.
func waitPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portFree(port) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return portFree(port)
}
