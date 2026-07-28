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
	case "schedule":
		target := arg("target")
		if target == "" {
			target = s.Self // default: wake myself
		}
		urgent := true // a scheduled wake fires the bubble now by default
		if _, present := p.Arguments["urgent"]; present {
			urgent = argBool("urgent")
		}
		id, err := s.B.Schedule(s.Self, target, arg("subject"), arg("body"), arg("every"), arg("daily"), urgent)
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "scheduled "+id+" — the daemon will wake "+target+" on this trigger, even while asleep")
	case "unschedule":
		if err := s.B.Unschedule(s.Self, arg("id")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "cancelled "+arg("id"))
	case "schedules":
		ss := s.B.Schedules(s.Self)
		if len(ss) == 0 {
			return toolOK(msg.ID, "(no schedules)")
		}
		return toolOK(msg.ID, strings.Join(ss, "\n"))
	case "mute":
		id, err := s.B.Mute(s.Self, arg("source"), arg("subject_re"), arg("body_re"), arg("window"), arg("ttl"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "muted "+id+" — matching messages will still land in inbox(), just without a notification/wake")
	case "unmute":
		if err := s.B.Unmute(s.Self, arg("id")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "unmuted "+arg("id"))
	case "mutes":
		ms := s.B.Mutes(s.Self)
		if len(ms) == 0 {
			return toolOK(msg.ID, "(no mute rules)")
		}
		return toolOK(msg.ID, strings.Join(ms, "\n"))
	case "webhook":
		target := arg("addr")
		if target == "" {
			target = s.Self
		}
		url, err := s.B.Webhook(s.Self, target)
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "incoming webhook for "+target+": "+url+"\nPOST JSON {\"subject\",\"body\",\"from\",\"urgent\"} or a raw body with ?subject= — it lands in its inbox and wakes it.")
	case "webhook_rotate":
		target := arg("addr")
		if target == "" {
			target = s.Self
		}
		url, err := s.B.WebhookRotate(s.Self, target)
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "rotated — new webhook for "+target+": "+url+" (the old URL is now dead)")
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
	case "control_webhook":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: control_webhook")
		}
		url, err := s.B.ControlWebhook(s.Self, argBool("rotate"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "control webhook (spawns/deletes as you — keep secret): "+url)
	case "assign_task":
		if !s.Spawnable {
			return errResp(msg.ID, -32601, "tool not available: assign_task")
		}
		id, err := s.B.AssignTask(s.Self, arg("to"), arg("brief"), arg("checklist"), argBool("deterministic"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "assigned "+id+" to "+arg("to")+" — you'll receive a '✅ task "+id+" verified' notice only after its contract passes; check tasks() for authoritative state")
	case "bubbles_port":
		base, err := s.B.Port()
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		if base == "" {
			return toolOK(msg.ID, "the webhook/HTTP server is not running (no port bound)")
		}
		port := base
		if i := strings.LastIndex(base, ":"); i >= 0 {
			port = base[i+1:]
		}
		return toolOK(msg.ID, "bubbles HTTP base "+base+" (port "+port+")")
	case "submit_task":
		out, err := s.B.SubmitTask(s.Self, arg("task_id"), arg("summary"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, out)
	case "verdict":
		out, err := s.B.Verdict(s.Self, arg("task_id"), argBool("pass"), arg("notes"))
		if err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, out)
	case "tasks":
		ts := s.B.Tasks(s.Self)
		if len(ts) == 0 {
			return toolOK(msg.ID, "(no tasks)")
		}
		return toolOK(msg.ID, strings.Join(ts, "\n"))
	case "cancel_task":
		if err := s.B.CancelTask(s.Self, arg("task_id")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "task "+arg("task_id")+" cancelled — worker stood down, verifier reaped")
	case "log_decision":
		if err := s.B.LogDecision(s.Self, arg("text")); err != nil {
			return toolErr(msg.ID, err.Error())
		}
		return toolOK(msg.ID, "logged to the decisions ledger")
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
