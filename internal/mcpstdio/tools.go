// Package mcpstdio is a minimal stdio JSON-RPC 2.0 MCP server exposing the
// bubble tools (send, contacts, spawn) to a claude session. One helper process
// per bubble; it relays tool calls to the main process via a Backend.
package mcpstdio

// Backend executes tool calls. The identity (from/by) is fixed by the Server to
// this bubble's own address, so a session cannot spoof another bubble.
type Backend interface {
	Send(from, to, subject, body string) error
	Contacts(owner string) []string
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
	ts := []Tool{
		{
			Name:        "send",
			Description: "Send a short message to a contact address (root is \"0\").",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": strProp("to", "subject", "body"),
				"required":   []string{"to", "subject"},
			},
		},
		{
			Name:        "contacts",
			Description: "List the addresses you are allowed to send to.",
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
