package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sentiolabs/observational-memory/internal/ledger"
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
	if value["protocol_version"] != float64(2) || value["ledger_schema"] != float64(2) {
		t.Fatal("wrong protocol")
	}
	for _, args := range [][]string{{"check-compatibility", "--protocol", "1", "--client", "codex"}, {"hook"}, {"hook", "--client", "claude-code"}, {"status"}} {
		if _, err = run(t, args, ""); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err = run(t, []string{"check-compatibility", "--protocol", "2", "--client", "codex"}, ""); err != nil {
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

func TestV2MutationContract(t *testing.T) {
	base := []string{"--store", t.TempDir(), "--session", "v2"}
	for _, input := range []string{`{"kind":"user","text":"evidence","key":null}`, `{"kind":"user","text":"evidence","key":"","unknown":true}`} {
		if _, err := run(t, append(base, "capture"), input); err == nil {
			t.Fatal("accepted invalid capture", input)
		}
	}
	output, err := run(t, append(base, "capture"), `{"kind":"user","text":"Correction","key":"","root_turn_id":"root","origin_completion_ordinal":2,"source_incomplete":true}`)
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		SourceID    string `json:"source_id"`
		FirstUnitID string `json:"first_unit_id"`
		UnitCount   int64  `json:"unit_count"`
	}
	if err = json.Unmarshal([]byte(output), &captured); err != nil {
		t.Fatal(err)
	}
	if captured.UnitCount != 1 || !strings.HasPrefix(captured.FirstUnitID, "e-") {
		t.Fatal("missing evidence capture receipt", output)
	}
	for _, payload := range []string{
		`{"through":1,"observations":[]}`,
		`{"expected_through":0,"expected_revision":0,"acknowledge":[],"observations":[{"text":"legacy","source_ids":["s-legacy"]}]}`,
		`{"expected_through":0,"acknowledge":[]}`,
		`{"expected_through":0,"expected_revision":0}`,
	} {
		if _, err = run(t, append(base, "apply"), payload); err == nil {
			t.Fatal("accepted legacy or incomplete mutation", payload)
		}
	}
	payload := `{"expected_through":0,"expected_revision":0,"acknowledge":["` + captured.FirstUnitID + `"],"working_state":{"objective":{"text":"Current correction","evidence_ids":["` + captured.FirstUnitID + `"]}}}`
	first, err := run(t, append(base, "apply"), payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := run(t, append(base, "apply"), payload)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) > 12000 || !strings.HasSuffix(first, "\n") || !json.Valid([]byte(first)) {
		t.Fatal("mutation receipt is not bounded stable JSON")
	}
	if _, err = run(t, append(base, "apply"), `{"expected_through":0,"expected_revision":0,"acknowledge":[]}`); err == nil {
		t.Fatal("accepted stale writer")
	}
}

func TestReadEnvelopeCLIReconstruction(t *testing.T) {
	base := []string{"--store", t.TempDir(), "--session", "bounded"}
	text := strings.Repeat("界🙂\x00\n\"\\", 4000)
	payload, err := json.Marshal(map[string]any{"kind": "tool", "text": text, "key": "large"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, append(base, "capture"), string(payload))
	if err != nil {
		t.Fatal(err)
	}
	var captured ledger.CaptureReceipt
	if err = json.Unmarshal([]byte(out), &captured); err != nil {
		t.Fatal(err)
	}
	reconstructed := ""
	token := ""
	seen := map[string]bool{}
	for {
		args := append(append([]string{}, base...), "recall", captured.SourceID)
		if token != "" {
			args = append(args, "--cursor", token)
		}
		out, err = run(t, args, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > ledger.ReadEnvelopeBytes || !utf8.ValidString(out) || !json.Valid([]byte(out)) || !strings.HasSuffix(out, "\n") {
			t.Fatal("invalid complete stdout")
		}
		var page ledger.RecallPage
		if err = json.Unmarshal([]byte(out), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Page.Items {
			if item.Evidence == nil || item.Evidence.StartByte != int64(len(reconstructed)) {
				t.Fatal("lost evidence span")
			}
			reconstructed += item.Evidence.Text
		}
		if page.Page.NextCursor == "" {
			break
		}
		if seen[page.Page.NextCursor] {
			t.Fatal("nonadvancing cursor")
		}
		token = page.Page.NextCursor
		seen[token] = true
	}
	if reconstructed != text {
		t.Fatal("stdout source reconstruction changed")
	}
	for _, args := range [][]string{{"prime", "--cursor", token}, {"view", "--cursor", token}, {"view", "--all", "--cursor", token}, {"pending", "--cursor", "bad"}} {
		out, err = run(t, append(append([]string{}, base...), args...), "")
		if err == nil || out != "" {
			t.Fatal("invalid cursor wrote partial output", args, err)
		}
	}
	cmd := New()
	var writer bytes.Buffer
	cmd.SetOut(&writer)
	if err = output(cmd, map[string]string{"oversize": strings.Repeat("\x00", 12000)}); err == nil || writer.Len() != 0 {
		t.Fatal("oversized response partially written")
	}
}
