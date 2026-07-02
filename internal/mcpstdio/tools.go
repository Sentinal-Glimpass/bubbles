// Package mcpstdio is a minimal stdio JSON-RPC 2.0 MCP server exposing the
// bubble tools (send, contacts, spawn) to a claude session. One helper process
// per bubble; it relays tool calls to the main process via a Backend.
package mcpstdio

// Backend executes tool calls. The identity (from/by) is fixed by the Server to
// this bubble's own address, so a session cannot spoof another bubble.
type Backend interface {
	Send(from, to, subject, body string, replyTo int, urgent bool) (int, error)
	Contacts(owner string) []string
	Inbox(owner string) []string
	Status(owner string) []string
	Spawn(by, name, description, dir, model string) (string, error)
	Edit(by, addr, name, description, model string) error
	Delete(by, addr string) (int, error) // returns how many bubbles were removed (target + subtree)
	Forget(by, addr string) error        // drop a contact from the caller's own list
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
	sendProps["urgent"] = map[string]any{
		"type":        "boolean",
		"description": "If true, wake the recipient immediately. If false (default), the message is pooled and delivered in the next drain cycle — use false unless a timely reply is needed.",
	}
	ts := []Tool{
		{
			Name:        "send",
			Description: "Send a message to a contact's inbox (root is \"0\"). Returns the message id; they read it via inbox(). By default it's pooled and delivered in batches; pass urgent=true for an immediate reply.",
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
		{
			Name:        "forget",
			Description: "Remove an address from YOUR contacts (you'll no longer be able to message it). Use to tidy up contacts you no longer need. You can't forget root (\"0\").",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"addr": map[string]any{
					"type":        "string",
					"description": "The contact address to drop, e.g. \"0.2\".",
				}},
				"required": []string{"addr"},
			},
		},
	}
	if s.Spawnable {
		spawnProps := strProp("dir")
		spawnProps["name"] = map[string]any{
			"type":        "string",
			"description": "Short name/label for the child bubble (a few words), shown in the fleet — NOT the instructions.",
		}
		spawnProps["description"] = map[string]any{
			"type":        "string",
			"description": "The child's initial instruction / charter — its first prompt (put the full task here, not in name).",
		}
		spawnProps["model"] = map[string]any{
			"type":        "string",
			"description": "Optional model for the child: \"sonnet\" (default), \"opus\", or \"fable\".",
		}
		ts = append(ts, Tool{
			Name:        "spawn",
			Description: "Spawn a child bubble (only if you were granted this). Give it a short 'name' and its task in 'description'.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": spawnProps,
				"required":   []string{"name"},
			},
		})
		editProps := strProp("name", "description", "model")
		editProps["addr"] = map[string]any{
			"type":        "string",
			"description": "Address of the sub-bubble to edit (must be in YOUR subtree, e.g. one you spawned).",
		}
		ts = append(ts, Tool{
			Name:        "edit",
			Description: "Edit one of YOUR sub-bubbles: change its name, model, or description (charter). Omitted fields are unchanged. Only works on bubbles you spawned (your subtree).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": editProps,
				"required":   []string{"addr"},
			},
		})
		ts = append(ts, Tool{
			Name:        "delete",
			Description: "Delete one of YOUR sub-bubbles (and everything under it). Only works on bubbles you spawned (your subtree). This is permanent — its session is killed and its address removed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"addr": map[string]any{
					"type":        "string",
					"description": "Address of the sub-bubble to delete (must be in YOUR subtree).",
				}},
				"required": []string{"addr"},
			},
		})
	}
	return ts
}
