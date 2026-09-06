package codex

import (
	"context"
	"database/sql"
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
	result, err := Handle(context.Background(), e, store, "/example with spaces/om")
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
	cp := capturedCheckpoint(t, l)
	cp.Observations = []ledger.ObservationV2{{Text: "Keep the API stable.", EvidenceIDs: []string{cp.Acknowledge[0]}}}
	_, err = l.ApplyV2(cp)
	if err != nil {
		t.Fatal(err)
	}
	start := event("SessionStart")
	start["source"] = "compact"
	restored := fmt.Sprint(invoke(t, store, start))
	if !strings.Contains(restored, " prime before continuing") || strings.Contains(restored, "Keep the API stable.") {
		t.Fatal("compaction hook must direct prime without embedding memory")
	}
	primed, err := l.Prime()
	if err != nil || !strings.Contains(primed, "Keep the API stable.") {
		t.Fatal("prime did not restore memory", err)
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
	tool["tool_name"] = "Bash"
	tool["tool_input"] = map[string]any{"command": "'/example with spaces/om' --session hooks status"}
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

// Use stored evidence identities and current status; source sequence numbers do
// not describe checkpoint progress once a source spans several evidence units.
func capturedCheckpoint(t *testing.T, l *ledger.Ledger) ledger.CheckpointV2 {
	t.Helper()
	status, err := l.Status()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", l.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT id FROM evidence_units WHERE review_state='pending' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cp := ledger.CheckpointV2{ExpectedThrough: status.Through, ExpectedRevision: status.Revision, Acknowledge: []string{}}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		cp.Acknowledge = append(cp.Acknowledge, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cp
}
