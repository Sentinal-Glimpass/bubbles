package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
)

// spike launches a command in a PTY, types a prompt, then mid-run sends an
// interrupt byte and a follow-up, so we can observe whether interrupt-delivery
// into a live, turn-based session works.
func main() {
	cmdName := flag.String("cmd", "claude", "command to launch in a PTY")
	prompt := flag.String("prompt", "write a long haiku about the sea", "initial input")
	intByte := flag.Int("int", 0x03, "interrupt byte to send (0x03=Ctrl-C, 0x1b=ESC)")
	flag.Parse()

	c := exec.Command(*cmdName, flag.Args()...)
	ptmx, err := pty.Start(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pty start:", err)
		os.Exit(1)
	}
	defer func() { _ = ptmx.Close() }()

	go func() { _, _ = io.Copy(os.Stdout, ptmx) }()

	time.Sleep(2 * time.Second)
	fmt.Println("\n--- injecting prompt ---")
	_, _ = ptmx.Write([]byte(*prompt + "\r"))

	time.Sleep(3 * time.Second)
	fmt.Printf("\n--- sending interrupt byte 0x%02x ---\n", *intByte)
	_, _ = ptmx.Write([]byte{byte(*intByte)})

	time.Sleep(1 * time.Second)
	fmt.Println("\n--- injecting follow-up after interrupt ---")
	_, _ = ptmx.Write([]byte("actually, write about mountains instead\r"))

	time.Sleep(6 * time.Second)
	fmt.Println("\n--- spike done ---")
	// claude enables mouse reporting / bracketed paste in the PTY; disable them
	// on our real terminal so the shell isn't left echoing mouse-move escapes.
	fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l")
}
