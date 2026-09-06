package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, args []string, input string) (string, error) {
	t.Helper()
	t.Setenv("OBSERVATIONAL_MEMORY_STORE", "")
	cmd := New()
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(input))
	var out, diagnostics bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostics)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}
func TestCompatibilityAndExplicitClient(t *testing.T) {
	output, err := run(t, []string{"capabilities"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal([]byte(output), &value); err != nil {
		t.Fatal(err)
	}
	if value["protocol_version"] != float64(1) {
		t.Fatal("wrong protocol")
	}
	for _, args := range [][]string{{"check-compatibility", "--protocol", "2", "--client", "codex"}, {"hook"}, {"hook", "--client", "claude-code"}, {"status"}} {
		if _, err = run(t, args, ""); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err = run(t, []string{"check-compatibility", "--protocol", "1", "--client", "codex"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = run(t, []string{"self", "update", "--help"}, ""); err != nil {
		t.Fatal(err)
	}
}
func TestJSONValidationAndHookErrors(t *testing.T) {
	store := t.TempDir()
	base := []string{"--store", store, "--session", "manual"}
	for _, payload := range []string{`{"through":0,"unexpected":true}`, `{}`, `null`, `{"through":0} {"through":0}`, `{"through":0.5}`} {
		if _, err := run(t, append(base, "apply"), payload); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
	output, err := run(t, []string{"--store", store, "hook", "--client", "codex"}, "invalid json")
	if err != nil || !strings.Contains(output, "systemMessage") {
		t.Fatalf("hook failure blocked task: %s %v", output, err)
	}
	output, err = run(t, append(base, "capture"), `{"kind":"user","text":"Exact evidence.","key":"test"}`)
	if err != nil || !strings.Contains(output, "source_id") {
		t.Fatal(output, err)
	}
	output, err = run(t, append(base, "pending"), "")
	if err != nil || !strings.Contains(output, "Exact evidence.") {
		t.Fatal(output, err)
	}
	if _, err = run(t, append(base, "capture"), `{"kind":"user","text":"Missing origin."}`); err == nil {
		t.Fatal("accepted missing capture key")
	}
}
