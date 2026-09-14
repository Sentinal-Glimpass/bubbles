package mcpstdio

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A mock child MCP server, run as a subprocess of the test binary via the
// re-exec pattern (TestMain below). It speaks just enough MCP to be proxied:
// initialize, tools/list (one tool "ping"), tools/call (ping -> "pong <arg>").
func runMockMCP() {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if dec.Decode(&msg) != nil {
			return
		}
		if len(msg.ID) == 0 {
			continue // notification (e.g. notifications/initialized)
		}
		switch msg.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mock", "version": "0"},
			}})
		case "tools/list":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"tools": []map[string]any{{
					"name":        "ping",
					"description": "returns pong",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
				}},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			m, _ := p.Arguments["msg"].(string)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "pong " + m}},
				"isError": false,
			}})
		default:
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "no"}})
		}
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("MOCK_MCP") == "1" {
		runMockMCP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// mockServerArgs launches this test binary as the mock MCP.
func mockServerArgs() (cmd string, args []string, env map[string]string) {
	return os.Args[0], nil, map[string]string{"MOCK_MCP": "1"}
}

// drive runs the server against a scripted set of requests and returns every
// message it wrote (responses + notifications), decoded.
func drive(t *testing.T, s *Server, requests []string) []map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		for _, r := range requests {
			_, _ = inW.Write([]byte(r + "\n"))
			time.Sleep(30 * time.Millisecond) // let each be handled (and any child round-trip finish)
		}
		time.Sleep(150 * time.Millisecond)
		_ = inW.Close()
	}()
	done := make(chan struct{})
	go func() { _ = s.Serve(inR, outW); outW.Close(); close(done) }()

	var msgs []map[string]any
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	<-done
	return msgs
}

// TestAddMCPMakesToolsLiveWithoutRestart is the whole feature end to end: a real
// child MCP is spawned via add_mcp, its tool shows up in a subsequent tools/list
// under the namespace, a proxied call reaches the child and returns its result,
// and a tools/list_changed notification is emitted so claude refetches live.
func TestAddMCPMakesToolsLiveWithoutRestart(t *testing.T) {
	s := &Server{Self: "0.6", B: &fakeBackend{}}
	cmd, args, env := mockServerArgs()
	addArgs, _ := json.Marshal(map[string]any{
		"name": "mock", "command": cmd, "args": args, "env": env,
	})

	call := func(id int, name string, argsJSON string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, argsJSON)
	}
	msgs := drive(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		call(2, "add_mcp", string(addArgs)),
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		call(4, "mock.ping", `{"msg":"hi"}`),
	})

	// 1. a list_changed notification was emitted
	sawNotify := false
	for _, m := range msgs {
		if m["method"] == "notifications/tools/list_changed" {
			sawNotify = true
		}
	}
	if !sawNotify {
		t.Fatal("no notifications/tools/list_changed emitted — claude would not refresh, so the tool would NOT be live")
	}

	// 2. the second tools/list (id 3) includes the namespaced proxied tool
	list3 := responseByID(msgs, 3)
	if list3 == nil || !toolListHas(list3, "mock.ping") {
		t.Fatalf("second tools/list does not advertise mock.ping: %v", list3)
	}
	// ...and the FIRST list (before add) did not
	if list1 := responseByID(msgs, 1); toolListHas(list1, "mock.ping") {
		t.Fatal("mock.ping present before add_mcp — the test proves nothing")
	}

	// 3. the proxied call reached the child and returned its result
	callResp := responseByID(msgs, 4)
	if txt := resultText(callResp); !strings.Contains(txt, "pong hi") {
		t.Fatalf("proxied tools/call did not return the child's result; got %q", txt)
	}
}

// TestOverlayPersistAndReload proves a relaunch restores added MCPs: one Server
// adds mock and writes the overlay; a fresh Server pointed at the same overlay
// reconnects it on startup and lists the tool with no add_mcp call.
func TestOverlayPersistAndReload(t *testing.T) {
	overlay := filepath.Join(t.TempDir(), "mcp", "0.6.json")
	cmd, args, env := mockServerArgs()
	addArgs, _ := json.Marshal(map[string]any{"name": "mock", "command": cmd, "args": args, "env": env})

	s1 := &Server{Self: "0.6", B: &fakeBackend{}, OverlayPath: overlay}
	drive(t, s1, []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_mcp","arguments":%s}}`, string(addArgs)),
	})
	if _, err := os.Stat(overlay); err != nil {
		t.Fatalf("overlay not written: %v", err)
	}

	s2 := &Server{Self: "0.6", B: &fakeBackend{}, OverlayPath: overlay}
	msgs := drive(t, s2, []string{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`})
	if list := responseByID(msgs, 1); !toolListHas(list, "mock.ping") {
		t.Fatal("reloaded server did not restore the added MCP from the overlay")
	}
}

// TestAddMCPRejectsBadName: a name with a dot would break the "<server>.<tool>"
// split, so it must be refused rather than silently corrupt routing.
func TestAddMCPRejectsBadName(t *testing.T) {
	s := &Server{Self: "0.6", B: &fakeBackend{}}
	msgs := drive(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_mcp","arguments":{"name":"a.b","command":"true"}}}`,
	})
	r := responseByID(msgs, 1)
	if !isToolError(r) {
		t.Fatalf("a name with a dot must be rejected: %v", r)
	}
}

// helpers ------------------------------------------------------------

func responseByID(msgs []map[string]any, id float64) map[string]any {
	for _, m := range msgs {
		if v, ok := m["id"].(float64); ok && v == id {
			return m
		}
	}
	return nil
}

func toolListHas(resp map[string]any, name string) bool {
	if resp == nil {
		return false
	}
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	for _, tv := range tools {
		tm, _ := tv.(map[string]any)
		if tm["name"] == name {
			return true
		}
	}
	return false
}

func resultText(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var b strings.Builder
	for _, cv := range content {
		cm, _ := cv.(map[string]any)
		if s, ok := cm["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func isToolError(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	result, _ := resp["result"].(map[string]any)
	e, _ := result["isError"].(bool)
	return e
}
