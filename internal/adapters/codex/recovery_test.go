package codex

import (
	"fmt"
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
	pending, err := open(t, store).Pending()
	if err != nil || len(pending.Sources) != 1 || !strings.Contains(pending.Sources[0].Text, "tests failed") {
		t.Fatal("ordinary test evidence was excluded", err)
	}
}

func TestCompactAndResumeRequestPrimeWithBacklog(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	for i := range 12 {
		id, err := l.Capture("user", fmt.Sprintf("Evidence %d", i), fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		pending, err := l.Pending()
		if err != nil {
			t.Fatal(err)
		}
		_, err = l.Apply(ledger.Checkpoint{Through: &pending.Through, Observations: []ledger.Observation{{Text: fmt.Sprintf("Fact %d: %s", i, strings.Repeat("🙂", 450)), SourceIDs: []string{id}}}})
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
	pending, err := l.Pending()
	if err != nil || len(pending.Sources) != 1 || pending.Sources[0].Text != e["prompt"] {
		t.Fatal("resume did not retain only permitted evidence", err)
	}
}
