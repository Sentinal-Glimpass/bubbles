// Command bubbles is an agent-native terminal IDE: a zoomable fleet of claude
// sessions. With no args it runs the TUI; the hidden "mcp-stdio" subcommand is
// the per-bubble MCP helper that claude launches.
package main

import (
	"errors"
	"fmt"
	"os"

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

func (b *ipcBackend) Send(from, to, subject, body string, replyTo int) (int, error) {
	rep, err := b.c.Do(ipc.Request{Op: "send", From: from, To: to, Subject: subject, Body: body, ReplyTo: replyTo})
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

func (b *ipcBackend) Spawn(by, persona, dir string) (string, error) {
	rep, err := b.c.Do(ipc.Request{Op: "spawn", From: by, Persona: persona, Dir: dir})
	if err != nil {
		return "", err
	}
	if !rep.OK {
		return "", errors.New(rep.Err)
	}
	return rep.Addr, nil
}
