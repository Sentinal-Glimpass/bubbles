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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp-stdio" {
		runMCPStdio()
		return
	}
	runApp()
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

func (b *ipcBackend) Send(from, to, subject, body string) error {
	rep, err := b.c.Do(ipc.Request{Op: "send", From: from, To: to, Subject: subject, Body: body})
	if err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Err)
	}
	return nil
}

func (b *ipcBackend) Contacts(owner string) []string {
	rep, _ := b.c.Do(ipc.Request{Op: "contacts", From: owner})
	return rep.Contacts
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
