package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sentiolabs/observational-memory/internal/ledger"
)

func TestMemoryCommandIdentity(t *testing.T) {
	const executable = "/plugin with spaces/bin/om"
	for _, tc := range []struct {
		name, tool, command string
		self                bool
	}{
		{"absolute", "Bash", "'/plugin with spaces/bin/om' --store /data --session hooks status", true},
		{"quoted", "Bash", `"/plugin with spaces/bin/om" prime`, true},
		{"path", "Bash", "om --store /data --session hooks prime", true},
		{"heredoc", "Bash", "'/plugin with spaces/bin/om' apply <<'JSON'\n{\"through\":1}\nJSON\n", true},
		{"application script", "Bash", "sh scripts/run.sh test", false},
		{"search mention", "Bash", "rg '/plugin with spaces/bin/om' .", false},
		{"mixed work", "Bash", "'/plugin with spaces/bin/om' status; go test ./...", false},
		{"command substitution", "Bash", "'/plugin with spaces/bin/om' capture <<< \"$(cat file)\"", false},
		{"output file", "Bash", "'/plugin with spaces/bin/om' view > report.txt", false},
		{"different path scope", "Bash", "om --store /unrelated --session hooks status", false},
		{"edit", "Write", "'/plugin with spaces/bin/om' status", false},
		{"invalid", "Bash", "'unclosed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := event("PostToolUse")
			e["tool_name"] = tc.tool
			e["tool_input"] = map[string]any{"command": tc.command}
			if got := memoryCommand(e, executable, "/data", "hooks"); got != tc.self {
				t.Fatalf("self = %v, want %v", got, tc.self)
			}
		})
	}
}

func TestOrdinaryRunScriptEvidenceIsCaptured(t *testing.T) {
	store := t.TempDir()
	e := event("PostToolUse")
	e["tool_name"] = "Bash"
	e["tool_input"] = map[string]any{"command": "sh scripts/run.sh test"}
	e["tool_response"] = "Authentication tests failed."
	invoke(t, store, e)
	pending, err := open(t, store).ReadPending("")
	if err != nil || len(pending.Page.Items) != 1 || !strings.Contains(pending.Page.Items[0].Text, "tests failed") {
		t.Fatal("ordinary test evidence was excluded", err)
	}
}

func TestCompactAndResumeRequestPrimeWithBacklog(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	for i := range 12 {
		captured, err := l.CaptureV2(ledger.CaptureInput{Kind: "user", Text: fmt.Sprintf("Evidence %d", i), Key: fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		cp := capturedCheckpoint(t, l)
		cp.Observations = []ledger.ObservationV2{{Text: fmt.Sprintf("Fact %d: %s", i, strings.Repeat("🙂", 450)), EvidenceIDs: []string{captured.FirstUnitID}}}
		_, err = l.ApplyV2(cp)
		if err != nil {
			t.Fatal(err)
		}
	}
	prompt := event("UserPromptSubmit")
	prompt["prompt"] = "Uncheckpointed correction."
	invoke(t, store, prompt)
	invoke(t, store, event("Interrupt"))
	for _, reason := range []string{"compact", "resume"} {
		e := event("SessionStart")
		e["source"] = reason
		output := invoke(t, store, e)["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
		if len(output) > ContextLimit || strings.Contains(output, "🙂") || !strings.Contains(output, "Run prime before continuing") || !strings.Contains(output, "1 pending sources") || strings.Contains(output, "last checkpoint none") {
			t.Fatalf("bad %s prime reminder: %s", reason, output)
		}
	}
	prime, err := l.Prime()
	if err != nil || !strings.Contains(prime, "🙂") || !strings.Contains(prime, "does not cover the pending backlog") {
		t.Fatal("prime failed to restore prepared memory with backlog", err)
	}
}

func TestPausePrefixExcludesItsOwnPromptAndFollowingTools(t *testing.T) {
	store := t.TempDir()
	e := event("UserPromptSubmit")
	e["prompt"] = "[om:pause]\nSensitive excluded example."
	invoke(t, store, e)
	tool := event("PostToolUse")
	tool["tool_response"] = "Excluded tool content."
	invoke(t, store, tool)
	l := open(t, store)
	status, err := l.Status()
	if err != nil || !status.Paused || status.PendingSources != 0 {
		t.Fatal("explicit pause prefix captured excluded data", err)
	}
	if err := l.Pause(false); err != nil {
		t.Fatal(err)
	}
	e["prompt"] = "Remember this permitted decision."
	e["turn_id"] = "turn-2"
	invoke(t, store, e)
	pending, err := l.ReadPending("")
	if err != nil || len(pending.Page.Items) != 1 || pending.Page.Items[0].Text != e["prompt"] {
		t.Fatal("resume did not retain only permitted evidence", err)
	}
}

func TestRecoveryBoundedReminderAndFailure(t *testing.T) {
	for _, source := range []string{"startup", "resume", "compact"} {
		store := t.TempDir()
		promptRoot(t, store, "root", "Current correction")
		invoke(t, store, event("Interrupt"))
		e := event("SessionStart")
		e["source"] = source
		out, err := Handle(context.Background(), e, store, "/quoted ' path/om")
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := ledger.JSON(out)
		if err != nil || len(encoded)+1 > ContextLimit {
			t.Fatal("unbounded hook payload", len(encoded), err)
		}
		text := fmt.Sprint(out)
		for _, required := range []string{"at most one pending page", "apply one checkpoint", "pending/deferred coverage", "user's task", "Deferred logs remain searchable", "'\"'\"'"} {
			if !strings.Contains(text, required) {
				t.Fatalf("missing %q: %s", required, text)
			}
		}
		s, err := open(t, store).Status()
		if err != nil || s.SourceCount != 1 || s.Through != 0 {
			t.Fatal("interrupt/recovery altered evidence", s, err)
		}
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, name := range []string{"SessionStart", "Stop"} {
			out, err := Handle(context.Background(), event(name), blocked, "/om")
			if err == nil || out["decision"] == "block" {
				t.Fatal("unavailable store requested work", out, err)
			}
		}
	}
	data, err := os.ReadFile(blocked)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("failure mutated store", err)
	}
}
func TestRecoverySerializedContextLimit(t *testing.T) {
	e := event("SessionStart")
	out, err := Handle(context.Background(), e, t.TempDir(), strings.Repeat("\"", 6000))
	if err == nil {
		encoded, err := ledger.JSON(out)
		if err != nil || len(encoded)+1 > ContextLimit {
			t.Fatalf("serialized context exceeds limit: %d", len(encoded)+1)
		}
	}
}
