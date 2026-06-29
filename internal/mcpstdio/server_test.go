package mcpstdio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type fakeBackend struct {
	sends [][4]string
}

func (f *fakeBackend) Send(from, to, subject, body string) error {
	f.sends = append(f.sends, [4]string{from, to, subject, body})
	return nil
}
func (f *fakeBackend) Contacts(owner string) []string       { return []string{"0", "0.2"} }
func (f *fakeBackend) Inbox(owner string) []string          { return nil }
func (f *fakeBackend) Spawn(by, p, d string) (string, error) { return "0.1.1", nil }

func TestServeFlow(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"to":"0","subject":"hi","body":"there"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"bogus","arguments":{}}}`,
	}, "\n"))

	fb := &fakeBackend{}
	s := &Server{Self: "0.1", B: fb, Spawnable: false}
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	type resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	var resps []resp
	dec := json.NewDecoder(&out)
	for dec.More() {
		var r resp
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode resp: %v", err)
		}
		resps = append(resps, r)
	}
	if len(resps) != 4 { // notification produces no response
		t.Fatalf("got %d responses want 4", len(resps))
	}

	// id1: initialize advertises a protocol version.
	var initR struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(resps[0].Result, &initR)
	if initR.ProtocolVersion == "" {
		t.Fatal("initialize missing protocolVersion")
	}

	// id2: tools/list = send, contacts (no spawn when not Spawnable).
	var listR struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(resps[1].Result, &listR)
	var names []string
	for _, tdef := range listR.Tools {
		names = append(names, tdef.Name)
	}
	if strings.Join(names, ",") != "send,contacts,inbox" {
		t.Fatalf("tools = %v want [send contacts inbox]", names)
	}

	// id3: send succeeded and recorded identity = Self.
	var callR struct {
		IsError bool `json:"isError"`
	}
	json.Unmarshal(resps[2].Result, &callR)
	if callR.IsError {
		t.Fatal("send returned isError")
	}
	if len(fb.sends) != 1 || fb.sends[0] != [4]string{"0.1", "0", "hi", "there"} {
		t.Fatalf("backend sends = %v", fb.sends)
	}

	// id4: unknown tool = JSON-RPC error.
	if resps[3].Error == nil {
		t.Fatal("bogus tool should return a JSON-RPC error")
	}
}

func TestSpawnGated(t *testing.T) {
	s := &Server{Self: "0.1", B: &fakeBackend{}, Spawnable: true}
	if len(s.tools()) != 4 {
		t.Fatalf("spawnable server should advertise 4 tools (send, contacts, inbox, spawn), got %d", len(s.tools()))
	}
}
