package runner

import "strings"

// FleetDefaultModel is what a bubble runs when neither it nor the operator
// names a model. Every model bubbles launches is a 1M-context variant: a bare
// alias like "opus" means the 200k window in claude, and a bubble resumed into
// a smaller window than its conversation needs is a bubble that breaks.
const FleetDefaultModel = "claude-opus-4-8[1m]"

// modelAliases maps the short names used in the spawn/edit picker, the spawn
// MCP tool and `bubbles --model` onto concrete 1M-context ids. Every id here
// was verified to resolve with `claude --model <id> -p ...` before being added.
var modelAliases = map[string]string{
	"opus":     FleetDefaultModel,
	"opus-4-8": "claude-opus-4-8[1m]",
	"opus-4.8": "claude-opus-4-8[1m]",
	"opus-5":   "claude-opus-5[1m]",
	"opus-5-5": "claude-opus-5-5[1m]",
	"opus-5.5": "claude-opus-5-5[1m]",
	"fable":    "claude-fable-5[1m]",
	"fable-5":  "claude-fable-5[1m]",
	// Legacy: bubbles spawned when "sonnet" was the picker default still carry
	// it. Fleet policy is 1M-context opus-class only, so it resolves to the
	// fleet default rather than silently downgrading those bubbles.
	"sonnet": FleetDefaultModel,
}

// ResolveModel turns a stored or requested model name into the concrete id to
// pass as --model. known is false for a name it does not recognise; callers
// that take operator input (bubbles --model) must reject those, because an
// unresolvable id would make EVERY launch fail.
//
// Accepted: "" (fleet default), a picker alias ("opus", "opus-5", "fable"…),
// the informal "opus-4_8_1m" style (underscores and a trailing 1m are
// normalised away), or a full "claude-…" id, to which [1m] is added if absent.
func ResolveModel(name string) (id string, known bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return FleetDefaultModel, true
	}
	if strings.HasPrefix(n, "claude-") {
		if !strings.Contains(n, "[") {
			n += "[1m]"
		}
		return n, true
	}
	n = strings.ReplaceAll(n, "_", "-")
	n = strings.TrimSuffix(strings.TrimSuffix(n, "[1m]"), "-1m")
	if id, ok := modelAliases[n]; ok {
		return id, true
	}
	return FleetDefaultModel, false
}
