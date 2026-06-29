// Package mcpstdio is a minimal stdio JSON-RPC 2.0 MCP server exposing the
// bubble tools (send, contacts, spawn) to a claude session. One helper process
// per bubble; it relays tool calls to the main process via a Backend.
package mcpstdio

// Backend executes tool calls. The identity (from/by) is fixed by the Server to
// this bubble's own address, so a session cannot spoof another bubble.
type Backend interface {
	Send(from, to, subject, body string, replyTo int) (int, error)
	Contacts(owner string) []string
	Inbox(owner string) []string
	Status(owner string) []string
	Spawn(by, persona, dir string) (string, error)
}

// Tool is an MCP tool definition advertised by tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(props ...string) map[string]any {
	p := map[string]any{}
	for _, name := range props {
		p[name] = map[string]any{"type": "string"}
	}
	return p
}

// tools returns the tool list for this Server; spawn appears only when granted.
func (s *Server) tools() []Tool {
	sendProps := strProp("to", "subject", "body")
	sendProps["reply_to"] = map[string]any{
		"type":        "integer",
		"description": "Optional id of the inbox message you are replying to (marks it answered for the sender).",
	}
	ts := []Tool{
		{
			Name:        "send",
			Description: "Send a message to a contact's inbox (root is \"0\"). Returns the message id. They are notified and read it via inbox().",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": sendProps,
				"required":   []string{"to", "subject"},
			},
		},
		{
			Name:        "contacts",
			Description: "List who you can message, each as \"address (role)\".",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "inbox",
			Description: "Read and clear your unread messages. Each shows its id and the sender's address and role; reply with send(..., reply_to=<id>).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "status",
			Description: "Check the messages you've SENT: delivered / read, no reply / replied. Use before re-sending so you don't nag someone who already saw it.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
	if s.Spawnable {
		ts = append(ts, Tool{
			Name:        "spawn",
			Description: "Spawn a child bubble (only if you were granted this).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": strProp("persona", "dir"),
				"required":   []string{"persona"},
			},
		})
	}
	return ts
}
