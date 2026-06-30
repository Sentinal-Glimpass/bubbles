package control

import (
	"path/filepath"
	"testing"
)

func TestControlRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	ln, err := Serve(sock, func(r Request) Reply {
		switch r.Op {
		case "snapshot":
			return Reply{OK: true, Snapshot: &Snapshot{
				Bubbles: []BubbleInfo{{Addr: "0.1", Persona: "scout", Status: "working", Unread: 2}},
				Groups:  []GroupInfo{{Name: "team", Members: []string{"0.1"}}},
				Marks:   map[string]string{"1": "0.1"},
			}}
		case "spawn":
			return Reply{OK: true, Addr: "0.2"}
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

	rep, err := c.Do(Request{Op: "snapshot"})
	if err != nil || rep.Snapshot == nil || len(rep.Snapshot.Bubbles) != 1 ||
		rep.Snapshot.Bubbles[0].Persona != "scout" || rep.Snapshot.Bubbles[0].Unread != 2 {
		t.Fatalf("snapshot = %+v err=%v", rep, err)
	}
	if rep.Snapshot.Marks["1"] != "0.1" || len(rep.Snapshot.Groups) != 1 {
		t.Fatalf("snapshot marks/groups wrong: %+v", rep.Snapshot)
	}
	if rep, _ := c.Do(Request{Op: "spawn", Persona: "x"}); rep.Addr != "0.2" {
		t.Fatalf("spawn addr = %q", rep.Addr)
	}
	if rep, _ := c.Do(Request{Op: "bogus"}); rep.OK {
		t.Fatal("bogus op should not be OK")
	}
}
