// Package codex translates Codex lifecycle events into client-independent ledger operations.
package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sentiolabs/observational-memory/internal/ledger"
)

func field(event map[string]any, key string) string { s, _ := event[key].(string); return s }
func quote(s string) string                         { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func contextOutput(event, text string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}}
}
func Handle(ctx context.Context, event map[string]any, store, executable string) (map[string]any, error) {
	empty := map[string]any{}
	if event == nil {
		return nil, fmt.Errorf("hook input must be an object")
	}
	name := field(event, "hook_event_name")
	if name != "SessionStart" && name != "UserPromptSubmit" && name != "PostToolUse" && name != "Stop" {
		return empty, nil
	}
	if field(event, "agent_id") != "" {
		return empty, nil
	}
	session := field(event, "session_id")
	l, err := ledger.Open(ctx, store, session)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	paused, err := l.Paused()
	if err != nil {
		return nil, err
	}
	if paused {
		return empty, nil
	}
	canonical := filepath.Dir(filepath.Dir(l.Path))
	command := fmt.Sprintf("%s --store %s --session %s", quote(executable), quote(canonical), quote(session))
	guidance := "Use $observational-memory to checkpoint new decisions, constraints, corrections, completions and blockers before ending substantive work. Ledger command: " + command + ". Read pending and apply a grounded checkpoint; an empty observation list is valid for routine content. This ledger is scoped to this session. Follow the user's current memory preferences. "
	turn := field(event, "turn_id")
	switch name {
	case "SessionStart":
		view, err := l.View()
		if err != nil {
			return nil, err
		}
		return contextOutput(name, guidance+"\n"+view), nil
	case "UserPromptSubmit":
		prompt := field(event, "prompt")
		ownPrompt, err := l.State("stop_prompt")
		if err != nil {
			return nil, err
		}
		if prompt == ownPrompt {
			return contextOutput(name, guidance), nil
		}
		if strings.TrimSpace(prompt) != "" {
			id, err := l.Capture("user", prompt, turn)
			if err != nil {
				return nil, err
			}
			if err = l.SetState("prompt_id", id); err != nil {
				return nil, err
			}
		}
		return contextOutput(name, guidance), nil
	case "PostToolUse":
		input, err := ledger.JSON(event["tool_input"])
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(input), executable) || strings.Contains(string(input), "scripts/run.sh") {
			return empty, nil
		}
		text, err := ledger.JSON(map[string]any{"tool": event["tool_name"], "input": event["tool_input"], "response": event["tool_response"]})
		if err != nil {
			return nil, err
		}
		key := field(event, "tool_use_id")
		if key == "" {
			key = turn
		}
		if _, err = l.Capture("tool", string(text), key); err != nil {
			return nil, err
		}
		status, err := l.Status()
		if err != nil {
			return nil, err
		}
		cursor := strconv.FormatInt(status.Through, 10)
		notified, err := l.State("notified_cursor")
		if err != nil {
			return nil, err
		}
		if status.PendingChars >= 40000 && notified != cursor {
			if err = l.SetState("notified_cursor", cursor); err != nil {
				return nil, err
			}
			return contextOutput(name, guidance+"The observation checkpoint is due now."), nil
		}
		return empty, nil
	case "Stop":
		key := turn
		if key == "" {
			key, err = l.State("prompt_id")
			if err != nil {
				return nil, err
			}
		}
		previous, err := l.State("stop_turn")
		if err != nil {
			return nil, err
		}
		if event["stop_hook_active"] == true || key == "" || previous == key {
			return empty, nil
		}
		text := field(event, "last_assistant_message")
		if strings.TrimSpace(text) != "" {
			if _, err = l.Capture("assistant", text, key); err != nil {
				return nil, err
			}
		}
		status, err := l.Status()
		if err != nil {
			return nil, err
		}
		if status.PendingSources == 0 {
			return empty, nil
		}
		reason := guidance + "Perform one bounded memory pass now, then finish. If unavailable, skip it; do not retry indefinitely."
		if err = l.SetState("stop_turn", key); err != nil {
			return nil, err
		}
		if err = l.SetState("stop_prompt", reason); err != nil {
			return nil, err
		}
		return map[string]any{"decision": "block", "reason": reason}, nil
	}
	return empty, nil
}
