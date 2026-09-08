package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelemetryOptInPrivacyAndPause(t *testing.T) {
	store := t.TempDir()
	base := []string{"--store", store, "--session", "PRIVATE-TASK"}
	if _, err := run(t, append(base, "prime"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store, "telemetry")); !os.IsNotExist(err) {
		t.Fatal("off creates telemetry")
	}
	if _, err := run(t, append(base, "telemetry", "enable"), ""); err != nil {
		t.Fatal(err)
	}
	hook := []string{"--store", store, "hook", "--client", "codex"}
	event := `{"hook_event_name":"SessionStart","source":"compact","session_id":"PRIVATE-TASK","prompt":"PRIVATE-PROMPT","cwd":"/PRIVATE/PATH","model":"PRIVATE-MODEL"}`
	for range 2 {
		if _, err := run(t, hook, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, append(base, "prime"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, append(base, "telemetry", "feedback", "recovered"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, append(base, "telemetry", "feedback", "PRIVATE-TEXT"), ""); err == nil {
		t.Fatal("accepted free text")
	}
	out, err := run(t, append(base, "telemetry", "report"), "")
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err = json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report["observed_tasks"] != float64(1) || report["tasks_with_prime_after_compact_signal"] != float64(1) || report["unique_compactions"] != nil {
		t.Fatal(out)
	}
	if strings.Contains(out, "PRIVATE") {
		t.Fatal("private report", out)
	}
	data, err := os.ReadFile(filepath.Join(store, "telemetry", "events.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("private persisted")
	}
	if _, err = run(t, append(base, "pause"), ""); err != nil {
		t.Fatal(err)
	}
	before, err := run(t, append(base, "telemetry", "report"), "")
	if err != nil {
		t.Fatal(err)
	}
	run(t, hook, event)
	run(t, append(base, "prime"), "")
	after, _ := run(t, append(base, "telemetry", "report"), "")
	if before != after {
		t.Fatal("paused events written")
	}
	if _, err = run(t, append(base, "telemetry", "disable"), ""); err != nil {
		t.Fatal(err)
	}
	run(t, append(base, "resume"), "")
	run(t, hook, event)
	after, _ = run(t, append(base, "telemetry", "report"), "")
	var disabled map[string]any
	json.Unmarshal([]byte(after), &disabled)
	if disabled["observed_events"] != report["observed_events"] {
		t.Fatal(after)
	}
}

func TestTelemetryFailurePreservesNormalOutputAndErrors(t *testing.T) {
	store := t.TempDir()
	args := []string{"--store", store, "--session", "task"}
	run(t, append(args, "prime"), "")
	before, err := run(t, append(args, "prime"), "")
	if err != nil {
		t.Fatal(err)
	}
	run(t, append(args, "telemetry", "enable"), "")
	os.WriteFile(filepath.Join(store, "telemetry", "events.sqlite3"), []byte("corrupt"), 0600)
	after, err := run(t, append(args, "prime"), "")
	if err != nil || before != after {
		t.Fatal("telemetry changed prime", err)
	}
	if _, err = run(t, append(args, "capture"), `{"bad":"PRIVATE-ERROR"}`); err == nil {
		t.Fatal("original error lost")
	}
	for _, category := range []string{"PreCompact", "PostCompact"} {
		out, err := run(t, []string{"--store", store, "hook", "--client", "codex"}, `{"hook_event_name":"`+category+`","session_id":"task","turn_id":"turn","trigger":"auto"}`)
		if err != nil || strings.TrimSpace(out) != "{}" {
			t.Fatal(out, err)
		}
	}
}

func TestTelemetryPausePromptAndRecordedOperationFailure(t *testing.T) {
	store := t.TempDir()
	base := []string{"--store", store, "--session", "task"}
	run(t, append(base, "telemetry", "enable"), "")
	if _, err := run(t, append(base, "capture"), `{"kind":"wrong","text":"PRIVATE-ERROR","key":"private"}`); err == nil {
		t.Fatal("failure expected")
	}
	before, _ := run(t, append(base, "telemetry", "report"), "")
	var r map[string]any
	json.Unmarshal([]byte(before), &r)
	if r["errors"].(map[string]any)["capture"] != float64(1) {
		t.Fatal(before)
	}
	_, err := run(t, []string{"--store", store, "hook", "--client", "codex"}, `{"hook_event_name":"UserPromptSubmit","session_id":"task","prompt":"[om:pause] PRIVATE"}`)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := run(t, append(base, "telemetry", "report"), "")
	if before != after {
		t.Fatal("privacy pause prompt recorded")
	}
}
