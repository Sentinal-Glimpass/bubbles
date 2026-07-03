package mcpstdio

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ProtocolVersion is advertised in initialize. Confirm/bump against the
// installed claude on macOS (see Plan 2 Mac validation).
const ProtocolVersion = "2025-06-18"

// Server speaks newline-delimited JSON-RPC 2.0 on stdio for one bubble.
type Server struct {
	Self      string  // this bubble's address; forced as from/by
	B         Backend // relays to the kernel
	Spawnable bool    // whether the spawn tool is offered
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads JSON-RPC messages from in and writes responses to out until EOF.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var msg rpcMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.ID == nil {
			continue // notification: no response
		}
		if err := enc.Encode(s.handle(msg)); err != nil {
			return err
		}
	}
}

func (s *Server) handle(msg rpcMessage) rpcResponse {
	switch msg.Method {
	case "initialize":
		return ok(msg.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "bubbles", "version": "0.1.0"},
		})
	case "tools/list":
		return ok(msg.ID, map[string]any{"tools": s.tools()})
	case "tools/call":
		return s.call(msg)
	default:
		return errResp(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (s *Server) call(msg rpcMessage) rpcResponse {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return errResp(msg.ID, -32602, "invalid params")
	}
	arg := func(k string) string {
		if v, ok := p.Arguments[k].(string); ok {
			return v
		}
		return ""
	}
	argInt := func(k string) int {
		switch v := p.Arguments[k].(type) {
		case float64:
			return int(v)
		case string:
			n, _ := strconv.Atoi(v)
			return n
		}
		return 0
	}
	argBool := func(k string) bool {
		switch v := p.Arguments[k].(type) {
		case bool:
			return v
		case string:
			return v == "true" || v == "1"
		}
		return false
	}
	switch p.Name {
	case "send":
		id, err := s.B.Send(s.Self, arg("to"), arg("subject"), arg("body"), argInt("reply_to"), argBool("urgent"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		mode := "pooled"
		if argBool("urgent") {
			mode = "urgent"
		}
		return toolOK(msg.ID, fmt.Sprintf("delivered to %s's inbox (msg #%d, %s)", arg("to"), id, mode))
	case "contacts":
		return toolOK(msg.ID, strings.Join(s.B.Contacts(s.Self), ", "))
	case "inbox":
		msgs := s.B.Inbox(s.Self)
		if len(msgs) == 0 {
			return toolOK(msg.ID, "(inbox empty)")
		}
		return toolOK(msg.ID, strings.Join(msgs, "\n"))
	case "status":
		st := s.B.Status(s.Self)
		if len(st) == 0 {
			return toolOK(msg.ID, "(no sent messages)")
		}
		return toolOK(msg.ID, strings.Join(st, "\n"))
	case "forget":
		if err := s.B.Forget(s.Self, arg("addr")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "forgot "+arg("addr"))
	case "compact":
		if err := s.B.Compact(s.Self, arg("focus")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "compaction scheduled — it runs after this turn; keep going, your context will shrink")
	case "spawn":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: spawn")
		}
		a, err := s.B.Spawn(s.Self, arg("name"), arg("description"), arg("dir"), arg("model"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "spawned "+a)
	case "edit":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: edit")
		}
		if err := s.B.Edit(s.Self, arg("addr"), arg("name"), arg("description"), arg("model")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "edited "+arg("addr"))
	case "delete":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: delete")
		}
		n, err := s.B.Delete(s.Self, arg("addr"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, fmt.Sprintf("deleted %s (%d bubble(s) removed)", arg("addr"), n))
	case "introduce":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: introduce")
		}
		if err := s.B.Introduce(s.Self, arg("a"), arg("b")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, fmt.Sprintf("introduced %s <-> %s", arg("a"), arg("b")))
	case "broadcast":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: broadcast")
		}
		n, err := s.B.Broadcast(s.Self, arg("subject"), arg("body"), argBool("urgent"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, fmt.Sprintf("broadcast to %d bubble(s) in your subtree", n))
	default:
		return errResp(msg.ID, -32602, "unknown tool: "+p.Name)
	}
}

func ok(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// toolOK / toolErr return MCP tool results (tool failures are results with
// isError, not JSON-RPC errors).
func toolOK(id json.RawMessage, text string) rpcResponse {
	return ok(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	})
}

func toolErr(id json.RawMessage, text string) rpcResponse {
	return ok(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	})
}
