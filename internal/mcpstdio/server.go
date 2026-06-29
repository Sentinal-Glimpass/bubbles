package mcpstdio

import (
	"encoding/json"
	"io"
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
	switch p.Name {
	case "send":
		if err := s.B.Send(s.Self, arg("to"), arg("subject"), arg("body")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "delivered to "+arg("to")+"'s inbox")
	case "contacts":
		return toolOK(msg.ID, strings.Join(s.B.Contacts(s.Self), ", "))
	case "inbox":
		msgs := s.B.Inbox(s.Self)
		if len(msgs) == 0 {
			return toolOK(msg.ID, "(inbox empty)")
		}
		return toolOK(msg.ID, strings.Join(msgs, "\n"))
	case "spawn":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: spawn")
		}
		a, err := s.B.Spawn(s.Self, arg("persona"), arg("dir"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "spawned "+a)
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
