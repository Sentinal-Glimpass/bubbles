package ipc

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := Serve(sock, func(r Request) Reply {
		switch r.Op {
		case "send":
			return Reply{OK: true}
		case "contacts":
			return Reply{OK: true, Contacts: []string{"0", "0.2"}}
		case "spawn":
			return Reply{OK: true, Addr: "0.1.1"}
		default:
			return Reply{OK: false, Err: "unknown op"}
		}
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer ln.Close()

	c, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if rep, err := c.Do(Request{Op: "send", From: "0.1", To: "0", Subject: "hi"}); err != nil || !rep.OK {
		t.Fatalf("send reply = %+v err=%v", rep, err)
	}
	rep, err := c.Do(Request{Op: "contacts", From: "0.1"})
	if err != nil || len(rep.Contacts) != 2 {
		t.Fatalf("contacts reply = %+v err=%v", rep, err)
	}
	if rep, _ := c.Do(Request{Op: "spawn", From: "0.1", Persona: "x"}); rep.Addr != "0.1.1" {
		t.Fatalf("spawn addr = %q want 0.1.1", rep.Addr)
	}
	if rep, _ := c.Do(Request{Op: "bogus"}); rep.OK {
		t.Fatalf("bogus op should not be OK")
	}
}
