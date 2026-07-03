// Command bubbles is an agent-native terminal IDE: a zoomable fleet of claude
// sessions. With no args it runs the TUI; the hidden "mcp-stdio" subcommand is
// the per-bubble MCP helper that claude launches.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Sentinal-Glimpass/bubbles/internal/ipc"
	"github.com/Sentinal-Glimpass/bubbles/internal/mcpstdio"
)

// hostedMode is true when the app runs as the daemon's child: then `q` detaches
// the client (fleet keeps running) instead of quitting the process.
var hostedMode bool

// detachSentinel is emitted by the hosted app on `q`; the client sees it and
// detaches. It's an OSC string terminals ignore, so it never shows on screen.
const detachSentinel = "\x1b]6660;bubbles-detach\x07"

func main() {
	applyMessagePollingFlag() // --message_polling <minutes> -> env, inherited by the daemon + hosted child
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp-stdio":
			runMCPStdio()
			return
		case "daemon":
			runDaemon()
			return
		case "stop":
			runStop()
			return
		case "session-hook":
			if len(os.Args) > 2 {
				runSessionHook(os.Args[2])
			}
			return
		case "--hosted":
			hostedMode = true
			runApp() // child of the daemon: q detaches
			return
		case "--local":
			runApp() // no daemon: q quits
			return
		}
	}
	runClient() // default: attach to (or start) the persistent workspace daemon
}

// applyMessagePollingFlag reads `--message_polling <minutes>` and stashes it in
// an env var so the detached daemon and the hosted app inherit it (the flag is
// on the client invocation, but the drain runs in the hosted app).
func applyMessagePollingFlag() {
	for i := 0; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--message_polling" {
			if n, err := strconv.Atoi(os.Args[i+1]); err == nil && n > 0 {
				_ = os.Setenv("BUBBLES_MSG_POLL", os.Args[i+1])
			}
			return
		}
	}
}

// messagePollMinutes is the drain interval (minutes) from BUBBLES_MSG_POLL,
// defaulting to 10.
func messagePollMinutes() int {
	if v := os.Getenv("BUBBLES_MSG_POLL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}

// runSessionHook is a Claude Code hook helper: it reads the hook JSON on stdin
// and writes the live session_id to `file`. Wired into a bubble's session as a
// hook so bubbles always knows the CURRENT conversation id — even after an
// in-session /resume — and resumes that (not the stale launch id) next time.
func runSessionHook(file string) {
	data, _ := io.ReadAll(os.Stdin)
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(data, &p) == nil && p.SessionID != "" {
		_ = os.MkdirAll(filepath.Dir(file), 0o755)
		_ = os.WriteFile(file, []byte(p.SessionID), 0o644)
	}
	// hooks must exit 0 and emit nothing to avoid interfering with the session.
}

// runMCPStdio is the MCP server claude spawns for one bubble. It relays tool
// calls to the main process over the unix socket named in BUBBLE_SOCK.
func runMCPStdio() {
	self := os.Getenv("BUBBLE_ADDR")
	sock := os.Getenv("BUBBLE_SOCK")
	client, err := ipc.Dial(sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bubbles mcp-stdio: dial:", err)
		os.Exit(1)
	}
	defer client.Close()

	srv := &mcpstdio.Server{
		Self:      self,
		Spawnable: os.Getenv("BUBBLE_SPAWNABLE") == "1",
		B:         &ipcBackend{c: client},
	}
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bubbles mcp-stdio: serve:", err)
		os.Exit(1)
	}
}

// ipcBackend implements mcpstdio.Backend by relaying over the IPC socket.
type ipcBackend struct{ c *ipc.Client }

func (b *ipcBackend) Send(from, to, subject, body string, replyTo int, urgent bool) (int, error) {
	rep, err := b.c.Do(ipc.Request{Op: "send", From: from, To: to, Subject: subject, Body: body, ReplyTo: replyTo, Urgent: urgent})
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, errors.New(rep.Err)
	}
	return rep.ID, nil
}

func (b *ipcBackend) Contacts(owner string) []string {
	rep, _ := b.c.Do(ipc.Request{Op: "contacts", From: owner})
	return rep.Contacts
}

func (b *ipcBackend) Inbox(owner string) []string {
	rep, _ := b.c.Do(ipc.Request{Op: "inbox", From: owner})
	return rep.Messages
}

func (b *ipcBackend) Status(owner string) []string {
	rep, _ := b.c.Do(ipc.Request{Op: "status", From: owner})
	return rep.Messages
}

func (b *ipcBackend) Spawn(by, name, description, dir, model string) (string, error) {
	rep, err := b.c.Do(ipc.Request{Op: "spawn", From: by, Name: name, Description: description, Dir: dir, Model: model})
	if err != nil {
		return "", err
	}
	if !rep.OK {
		return "", errors.New(rep.Err)
	}
	return rep.Addr, nil
}

func (b *ipcBackend) Edit(by, addr, name, description, model string) error {
	rep, err := b.c.Do(ipc.Request{Op: "edit", From: by, Addr: addr, Name: name, Description: description, Model: model})
	if err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Err)
	}
	return nil
}

func (b *ipcBackend) Delete(by, addr string) (int, error) {
	rep, err := b.c.Do(ipc.Request{Op: "delete", From: by, Addr: addr})
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, errors.New(rep.Err)
	}
	return rep.ID, nil // ID doubles as the removed-count for delete
}

func (b *ipcBackend) Forget(by, addr string) error {
	rep, err := b.c.Do(ipc.Request{Op: "forget", From: by, Addr: addr})
	if err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Err)
	}
	return nil
}

func (b *ipcBackend) Introduce(by, a, bb string) error {
	rep, err := b.c.Do(ipc.Request{Op: "introduce", From: by, Addr: a, To: bb})
	if err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Err)
	}
	return nil
}

func (b *ipcBackend) Broadcast(by, subject, body string, urgent bool) (int, error) {
	rep, err := b.c.Do(ipc.Request{Op: "broadcast", From: by, Subject: subject, Body: body, Urgent: urgent})
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, errors.New(rep.Err)
	}
	return rep.ID, nil // ID = number of bubbles reached
}
