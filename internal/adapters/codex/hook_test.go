package codex

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sentiolabs/observational-memory/internal/ledger"
)

func event(name string) map[string]any {
	return map[string]any{"hook_event_name": name, "session_id": "hooks", "turn_id": "turn-1"}
}
func invoke(t *testing.T, store string, e map[string]any) map[string]any {
	t.Helper()
	result, err := Handle(context.Background(), e, store, "/example with spaces/observational-memory")
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func open(t *testing.T, store string) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(context.Background(), store, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}
func TestLifecycleAndContinuationExclusion(t *testing.T) {
	store := t.TempDir()
	prompt := event("UserPromptSubmit")
	prompt["prompt"] = "Keep this exact API constraint."
	result := invoke(t, store, prompt)
	if !strings.Contains(fmt.Sprint(result), "--session 'hooks'") {
		t.Fatal("missing session identity")
	}
	stop := event("Stop")
	stop["last_assistant_message"] = "Implementation completed; tests passed."
	stopped := invoke(t, store, stop)
	if stopped["decision"] != "block" {
		t.Fatal("missing checkpoint continuation")
	}
	if len(invoke(t, store, stop)) != 0 {
		t.Fatal("repeated Stop loop")
	}
	continuation := event("UserPromptSubmit")
	continuation["turn_id"] = "continuation"
	continuation["prompt"] = stopped["reason"]
	invoke(t, store, continuation)
	stop["turn_id"] = "another"
	stop["stop_hook_active"] = true
	if len(invoke(t, store, stop)) != 0 {
		t.Fatal("recursive Stop loop")
	}
	l := open(t, store)
	pending, err := l.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Sources) != 2 || pending.Sources[0].Text != prompt["prompt"] {
		t.Fatal("continuation entered source evidence")
	}
	through := pending.Through
	_, err = l.Apply(ledger.Checkpoint{Through: &through, Observations: []ledger.Observation{{Text: "Keep the API stable.", SourceIDs: []string{pending.Sources[0].ID}}}})
	if err != nil {
		t.Fatal(err)
	}
	start := event("SessionStart")
	start["source"] = "compact"
	if !strings.Contains(fmt.Sprint(invoke(t, store, start)), "Keep the API stable.") {
		t.Fatal("memory not restored after compaction")
	}
}
func TestExcludedEventsAndPause(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	if err := l.Pause(true); err != nil {
		t.Fatal(err)
	}
	prompt := event("UserPromptSubmit")
	prompt["prompt"] = "Do not retain this."
	if len(invoke(t, store, prompt)) != 0 {
		t.Fatal("ignored pause")
	}
	if err := l.Pause(false); err != nil {
		t.Fatal(err)
	}
	prompt["agent_id"] = "worker"
	invoke(t, store, prompt)
	tool := event("PostToolUse")
	tool["tool_input"] = map[string]any{"cmd": "/example with spaces/observational-memory --session hooks status"}
	invoke(t, store, tool)
	invoke(t, store, event("Unsupported"))
	pending, err := l.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Sources) != 0 {
		t.Fatal("captured excluded evidence")
	}
}
func TestThresholdReminderOncePerCursor(t *testing.T) {
	store := t.TempDir()
	tool := event("PostToolUse")
	tool["tool_response"] = strings.Repeat("a", 21000)
	for i := range 3 {
		tool["tool_use_id"] = fmt.Sprint(i)
		result := invoke(t, store, tool)
		_, reminder := result["hookSpecificOutput"]
		if reminder != (i == 1) {
			t.Fatalf("wrong reminder at %d: %v", i, result)
		}
	}
}
