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
	Compact(owner, focus string) error // compact the caller's own conversation now
	Schedule(by, target, subject, body, every, daily string, urgent bool) (string, error)
	Unschedule(by, id string) error
	Schedules(by string) []string
	Webhook(by, target string) (string, error)       // incoming-webhook URL for target (self or a bubble in by's subtree)
	WebhookRotate(by, target string) (string, error) // revoke + reissue target's URL (same authority)
	ControlWebhook(by string, rotate bool) (string, error) // control-webhook URL that spawns/deletes bubbles as the caller
	Port() (string, error)                                 // the daemon's HTTP/webhook base URL (embeds the current port)
	Spawn(by, name, description, dir, model string) (string, error)
	Edit(by, addr, name, description, model string) error
	Delete(by, addr string) (int, error) // returns how many bubbles were removed (target + subtree)
	Forget(by, addr string) error        // drop a contact from the caller's own list
	Introduce(by, a, b string) error     // make two of the caller's sub-bubbles mutual contacts
	Broadcast(by, subject, body string, urgent bool) (int, error) // message the caller's whole subtree

	// Harnessed tasks: assigned work travels worker → checks → (verifier) →
	// assigner, a route the kernel enforces.
	AssignTask(by, to, brief, checklist string, deterministic bool) (string, error) // checklist: newline-separated items
	SubmitTask(by, taskID, summary string) (string, error)
	Verdict(by, taskID string, pass bool, notes string) (string, error)
	Tasks(by string) []string
	CancelTask(by, taskID string) error // assigner withdraws a task (reaps its verifier)
	LogDecision(by, text string) error  // serialized append to the shared decisions ledger
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
		{
			Name:        "compact",
			Description: "Compact YOUR OWN conversation now to reclaim context. Call this at a natural checkpoint — a task is finished and you've written down anything important — instead of letting the context grow until it auto-compacts near the limit. Optionally pass 'focus' to steer what the summary keeps.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"focus": map[string]any{
					"type":        "string",
					"description": "Optional: what the summary should preserve, e.g. \"keep the API schema and open TODOs\".",
				}},
			},
		},
		{
			Name: "schedule",
			Description: "Set a DURABLE recurring wake: the daemon delivers a message to a bubble on a timer, waking it even if it (and you) are asleep — so it can be an event-driven worker without a permanently-running session. Target yourself or a bubble you own. Give EITHER every (\"15m\", \"2h\") OR daily (\"08:00\"). Returns a schedule id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target":  map[string]any{"type": "string", "description": "Bubble to wake (yourself or one in your subtree)."},
					"subject": map[string]any{"type": "string", "description": "Short subject shown on wake."},
					"body":    map[string]any{"type": "string", "description": "The task/instruction delivered on each wake."},
					"every":   map[string]any{"type": "string", "description": "Interval like \"15m\" or \"2h\" (min 30s). Use this OR daily."},
					"daily":   map[string]any{"type": "string", "description": "Local clock time \"HH:MM\" for a once-a-day wake. Use this OR every."},
					"urgent":  map[string]any{"type": "boolean", "description": "Wake immediately on fire (default true for schedules)."},
				},
				"required": []string{"target", "subject"},
			},
		},
		{
			Name:        "unschedule",
			Description: "Cancel a wake schedule by its id.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string", "description": "The schedule id from schedule()/schedules()."}},
				"required":   []string{"id"},
			},
		},
		{
			Name:        "schedules",
			Description: "List the wake schedules you can see (ones you created or that target you / your subtree).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "webhook",
			Description: "Get an incoming-webhook URL (minted on first call, stable afterwards) — yours by default, or a sub-bubble's via 'addr' (only bubbles in YOUR subtree). Anything that can make an HTTP request — a cron, a script, an external service — can POST to it and the payload lands in that bubble's inbox as a message from \"webhook\", waking it if asleep. POST JSON {subject, body, from?, urgent?} or a raw body with ?subject=. Webhook messages cannot be replied to.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"addr": map[string]any{
					"type":        "string",
					"description": "Optional: which bubble's webhook to fetch — yourself (default) or a bubble you own (your subtree).",
				}},
			},
		},
		{
			Name:        "webhook_rotate",
			Description: "Revoke a webhook URL and mint a fresh one (yours by default, or a sub-bubble's via 'addr'). Use if the URL leaked or a program should lose access; the old URL then gets 404.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"addr": map[string]any{
					"type":        "string",
					"description": "Optional: which bubble's webhook to rotate — yourself (default) or a bubble you own.",
				}},
			},
		},
		{
			Name:        "bubbles_port",
			Description: "Get the bubbles daemon's HTTP base URL and port — the port your webhook() and control_webhook() URLs are served on. Fixed at 3315 by default (override with `bubbles --port N`); a program that needs to build or verify a webhook URL can read the live port here.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "submit_task",
			Description: "Submit your completion of an assigned task. The kernel runs the task's check command in your dir FIRST — failures come straight back to you with output and nothing reaches the assigner. If the task has a checklist, your submission goes to its independent verifier; only an approved submission is delivered to the assigner as verified. This is the ONLY way to complete a task — a plain send() claiming completion is marked unverified.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "The task id from your assignment message, e.g. \"t3\"."},
					"summary": map[string]any{"type": "string", "description": "What you did — included in the verified completion the assigner receives."},
				},
				"required": []string{"task_id", "summary"},
			},
		},
		{
			Name:        "verdict",
			Description: "Rule on a task submission (task verifiers; assigners may override). pass=true delivers the verified completion to the assigner and closes the task; pass=false sends your notes to the worker as fix-it feedback and reopens the task.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "The task id, e.g. \"t3\"."},
					"pass":    map[string]any{"type": "boolean", "description": "true = approve, false = reject."},
					"notes":   map[string]any{"type": "string", "description": "Per-item findings; on reject, the SPECIFIC fixes the worker should make."},
				},
				"required": []string{"task_id", "pass", "notes"},
			},
		},
		{
			Name:        "tasks",
			Description: "List the tasks you participate in (as assigner, worker, or verifier) with their authoritative state — trust this over any completion claim in a message.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "cancel_task",
			Description: "Withdraw a task you assigned (assigners only). Stops it marking the worker's messages unverified and REAPS its independent verifier bubble — use this for a mis-assigned or superseded task so its verifier doesn't linger and burn tokens.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"task_id": map[string]any{"type": "string", "description": "The task id to cancel, e.g. \"t3\"."}},
				"required":   []string{"task_id"},
			},
		},
		{
			Name:        "log_decision",
			Description: "Append one durable decision to the fleet's shared ledger (.bubbles/memory/decisions.md): what was decided and why. The kernel serializes writes — use this instead of editing the file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"text": map[string]any{
					"type":        "string",
					"description": "One decision: \"<decision> — <why>\".",
				}},
				"required": []string{"text"},
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
		introProps := strProp()
		introProps["a"] = map[string]any{"type": "string", "description": "First sub-bubble address (in YOUR subtree)."}
		introProps["b"] = map[string]any{"type": "string", "description": "Second sub-bubble address (in YOUR subtree)."}
		ts = append(ts, Tool{
			Name:        "introduce",
			Description: "Introduce two of YOUR sub-bubbles to each other (mutual contacts), so they can hand off directly instead of relaying through you. Both must be in your subtree.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": introProps,
				"required":   []string{"a", "b"},
			},
		})
		bcProps := strProp("subject", "body")
		bcProps["urgent"] = map[string]any{"type": "boolean", "description": "Wake every recipient now (default false = pooled)."}
		ts = append(ts, Tool{
			Name:        "broadcast",
			Description: "Send one message to EVERY bubble in your subtree (all your descendants) at once — for fleet-wide notices to the workers you own. Returns how many were reached.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": bcProps,
				"required":   []string{"subject"},
			},
		})
		ts = append(ts, Tool{
			Name:        "control_webhook",
			Description: "Get YOUR control-webhook URL (minted on first call, stable afterwards) — a programmatic endpoint that spawns and deletes bubbles as you, without you being in the loop. Any script/cron can POST JSON to it: {\"action\":\"spawn\",\"name\":\"...\",\"description\":\"charter\",\"dir\":\"...\",\"model\":\"opus\"} returns the new address + its message webhook; {\"action\":\"delete\",\"target\":\"0.3.1\"} removes a bubble in your subtree; {\"action\":\"list\"} lists your children. Uses your spawn authority — treat the URL as a secret. Pass rotate=true to revoke the old URL and issue a fresh one.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{"rotate": map[string]any{
					"type":        "boolean",
					"description": "Revoke the current control URL and mint a new one (default false).",
				}},
			},
		})
		ts = append(ts, Tool{
			Name:        "assign_task",
			Description: "Assign a task with a kernel-enforced acceptance contract to a bubble in YOUR subtree, or to YOURSELF (self-imposed, still judged by an independent verifier). A checklist (one item per line) is required — an independent verifier bubble judges it. Set deterministic=true for the verifier to write+run test cases; false (default) for subjective judgement. The worker completes via submit_task; you receive a '✅ task verified' notice ONLY after the verifier approves — the worker cannot deliver an unverified 'done'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to":            map[string]any{"type": "string", "description": "Worker bubble's address (must be in your subtree)."},
					"brief":         map[string]any{"type": "string", "description": "The task: what to do and what done means."},
					"checklist":     map[string]any{"type": "string", "description": "Acceptance checklist, one item per line — verified by an independent verifier bubble."},
					"deterministic": map[string]any{"type": "boolean", "description": "true = verifier writes+runs test cases against the checklist; false (default) = subjective judgement."},
				},
				"required": []string{"to", "brief", "checklist"},
			},
		})
	}
	return ts
}
