package codex

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	prompt["prompt"] = strings.Repeat("Keep this exact API constraint. ", 1400)
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
	pending, err := l.ReadPending("")
	if err != nil {
		t.Fatal(err)
	}
	status, statusErr := l.Status()
	if statusErr != nil || status.SourceCount != 3 || !strings.HasPrefix(prompt["prompt"].(string), pending.Page.Items[0].Text) {
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
	pending, err := l.ReadPending("")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Page.Items) != 0 {
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
	cp := ledger.CheckpointV2{ExpectedThrough: status.Through, ExpectedRevision: status.Revision, Acknowledge: []string{}}
	token := ""
	for {
		page, err := l.ReadPending(token)
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range page.Page.Items {
			cp.Acknowledge = append(cp.Acknowledge, unit.ID)
		}
		if page.Page.NextCursor == "" {
			break
		}
		token = page.Page.NextCursor
	}
	return cp
}

func rootEvent(name, root string) map[string]any { e := event(name); e["turn_id"] = root; return e }
func promptRoot(t *testing.T, store, root, text string) {
	t.Helper()
	e := rootEvent("UserPromptSubmit", root)
	e["prompt"] = text
	invoke(t, store, e)
}
func stopRoot(t *testing.T, store, root, text string) map[string]any {
	t.Helper()
	e := rootEvent("Stop", root)
	e["last_assistant_message"] = text
	return invoke(t, store, e)
}
func TestStopCadence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint bool
		tool, tail string
		block      bool
	}{
		{name: "one small routine exchange causes no continuation", tail: "Done."},
		{name: "already checkpointed pre-final work leaves only final tail", checkpoint: true, tail: "Done."},
		{name: "huge new final tail alone still causes no continuation", tail: strings.Repeat("界", 20000)},
		{name: "pre-final pending bytes reach 40000", tool: strings.Repeat("界", 14000), tail: "Done.", block: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := t.TempDir()
			invoke(t, store, event("SessionStart"))
			promptRoot(t, store, "one", "Routine work")
			if tc.tool != "" {
				e := rootEvent("PostToolUse", "one")
				e["tool_use_id"] = "tool"
				e["tool_response"] = tc.tool
				invoke(t, store, e)
			}
			l := open(t, store)
			if tc.checkpoint {
				if _, err := l.ApplyV2(capturedCheckpoint(t, l)); err != nil {
					t.Fatal(err)
				}
			}
			got := stopRoot(t, store, "one", tc.tail)
			if (got["decision"] == "block") != tc.block {
				t.Fatalf("Stop=%v", got)
			}
			var recovered strings.Builder
			token := ""
			for {
				page, err := l.ReadPending(token)
				if err != nil {
					t.Fatal(err)
				}
				for _, unit := range page.Page.Items {
					if unit.Kind == "assistant" {
						recovered.WriteString(unit.Text)
					}
				}
				if page.Page.NextCursor == "" {
					break
				}
				token = page.Page.NextCursor
			}
			if recovered.String() != tc.tail {
				t.Fatal("final source cannot be reconstructed exactly")
			}
			if len(stopRoot(t, store, "one", tc.tail)) != 0 {
				t.Fatal("duplicate Stop continued")
			}
			s, err := l.Status()
			if err != nil || s.OldestPendingAgeTurns != 0 {
				t.Fatal("duplicate aged", s, err)
			}
		})
	}
}
func TestStopBacklogAge(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	for i := 0; i < 4; i++ {
		root := fmt.Sprint(i)
		promptRoot(t, store, root, "same prompt")
		got := stopRoot(t, store, root, "same final")
		if (got["decision"] == "block") != (i == 3) {
			t.Fatalf("completion %d: %v", i, got)
		}
		if i == 1 {
			cp := capturedCheckpoint(t, l)
			cp.Acknowledge = cp.Acknowledge[:1]
			if _, err := l.ApplyV2(cp); err != nil {
				t.Fatal(err)
			}
		}
		s, err := l.Status()
		if err != nil || s.OldestPendingAgeTurns != int64(i) {
			t.Fatalf("age %d: %+v %v", i, s, err)
		}
	}
	s, _ := l.Status()
	if s.SourceCount != 8 {
		t.Fatal("distinct turns deduplicated", s.SourceCount)
	}
}
func TestStopRecursionCapturesSyntheticFinal(t *testing.T) {
	store := t.TempDir()
	promptRoot(t, store, "root", strings.Repeat("work", 11000))
	first := stopRoot(t, store, "root", "Initial final")
	reason, ok := first["reason"].(string)
	if !ok {
		t.Fatal("no continuation", first)
	}
	promptRoot(t, store, "synthetic", reason)
	e := rootEvent("Stop", "synthetic")
	e["stop_hook_active"] = true
	e["last_assistant_message"] = "Synthetic final 🙂\nexact"
	for range 2 {
		if len(invoke(t, store, e)) != 0 {
			t.Fatal("recursive continuation")
		}
	}
	l := open(t, store)
	s, err := l.Status()
	if err != nil || s.SourceCount != 3 || s.OldestPendingAgeTurns != 0 {
		t.Fatal("synthetic capture or age", s, err)
	}
	hits, err := l.ReadSearch("Synthetic", ledger.SearchOptions{Sources: true})
	if err != nil || len(hits.Page.Items) != 1 || hits.Page.Items[0].Evidence.Text != e["last_assistant_message"] {
		t.Fatal("synthetic final lost", hits, err)
	}
	promptRoot(t, store, "real-next", reason+" A real request.")
	stopRoot(t, store, "real-next", "Real final")
	s, _ = l.Status()
	if s.SourceCount != 5 || s.OldestPendingAgeTurns != 1 {
		t.Fatal("similar real prompt excluded", s)
	}
}

func TestStopRecursionConcurrentDuplicates(t *testing.T) {
	for _, backlog := range []bool{false, true} {
		t.Run(fmt.Sprint(backlog), func(t *testing.T) {
			store := t.TempDir()
			prompt := "Small work"
			if backlog {
				prompt = strings.Repeat("work", 11000)
			}
			promptRoot(t, store, "one", prompt)
			const workers = 8
			ready := make(chan struct{})
			outputs := make(chan map[string]any, workers)
			errors := make(chan error, workers)
			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-ready
					e := rootEvent("Stop", "one")
					e["last_assistant_message"] = strings.Repeat("界", 20000)
					out, err := Handle(context.Background(), e, store, "/example with spaces/om")
					outputs <- out
					errors <- err
				}()
			}
			close(ready)
			wg.Wait()
			close(outputs)
			close(errors)
			for err := range errors {
				if err != nil {
					t.Error(err)
				}
			}
			blocks := 0
			for out := range outputs {
				if out["decision"] == "block" {
					blocks++
				}
			}
			want := 0
			if backlog {
				want = 1
			}
			if blocks != want {
				t.Fatalf("blocks=%d want %d", blocks, want)
			}
			s, err := open(t, store).Status()
			if err != nil || s.SourceCount != 2 || s.OldestPendingAgeTurns != 0 {
				t.Fatal("duplicate capture/completion", s, err)
			}
		})
	}
}
func TestStopCadenceDeferredDebtAndManualAge(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	tool, err := l.CaptureV2(ledger.CaptureInput{Kind: "tool", Text: strings.Repeat("界", 20000), Key: "manual log"})
	if err != nil {
		t.Fatal(err)
	}
	cp := capturedCheckpoint(t, l)
	cp.Acknowledge = []string{}
	cp.DeferSources = []ledger.SourceDeferral{{SourceID: tool.SourceID, Reason: "routine historical log"}}
	if _, err = l.ApplyV2(cp); err != nil {
		t.Fatal(err)
	}
	_, err = l.CaptureV2(ledger.CaptureInput{Kind: "user", Text: "manual correction", Key: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		root := fmt.Sprint(i)
		promptRoot(t, store, root, "Routine")
		out := stopRoot(t, store, root, "Done")
		if (out["decision"] == "block") != (i == 3) {
			t.Fatalf("manual age %d: %v", i, out)
		}
	}
	s, err := l.Status()
	if err != nil || s.Coverage.Deferred.Bytes != 60000 || s.OldestPendingAgeTurns != 3 {
		t.Fatal("deferred coverage or manual age", s, err)
	}
}
func TestStopCadenceMissingIdentityAndExclusions(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	for _, name := range []string{"UserPromptSubmit", "PostToolUse", "Stop", "Interrupt"} {
		e := event(name)
		e["agent_id"] = "subagent"
		e["prompt"] = "excluded"
		e["tool_response"] = "excluded"
		e["last_assistant_message"] = "excluded"
		invoke(t, store, e)
	}
	s, err := l.Status()
	if err != nil || s.SourceCount != 0 {
		t.Fatal("subagent capture", s, err)
	}
	e := rootEvent("Stop", "")
	e["last_assistant_message"] = "Unattributed final"
	for range 2 {
		if len(invoke(t, store, e)) != 0 {
			t.Fatal("unidentified continuation")
		}
	}
	n, err := l.State("root_completed_ordinal")
	if err != nil || n != "0" {
		t.Fatal("fabricated root", n, err)
	}
	promptRoot(t, store, "", "No host turn identifier")
	stopRoot(t, store, "", "Captured final")
	stopRoot(t, store, "", "Captured final")
	n, err = l.State("root_completed_ordinal")
	if err != nil || n != "1" {
		t.Fatal("missing captured prompt fallback", n, err)
	}
	promptID, err := l.State("prompt_id")
	if err != nil {
		t.Fatal(err)
	}
	ordinal, err := l.CompleteRootTurn(promptID)
	if err != nil || ordinal != 1 {
		t.Fatal("completion did not use captured prompt identity", ordinal, err)
	}
	s, err = l.Status()
	if err != nil || s.SourceCount != 3 {
		t.Fatal("unattributed final not retained exactly once", s, err)
	}
}

func TestThresholdReminderPendingBytesAndDeferral(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	captured, err := l.CaptureV2(ledger.CaptureInput{Kind: "tool", Text: strings.Repeat("old", 20000), Key: "old"})
	if err != nil {
		t.Fatal(err)
	}
	cp := capturedCheckpoint(t, l)
	cp.Acknowledge = []string{}
	cp.DeferSources = []ledger.SourceDeferral{{SourceID: captured.SourceID, Reason: "historical log"}}
	if _, err = l.ApplyV2(cp); err != nil {
		t.Fatal(err)
	}
	tool := event("PostToolUse")
	tool["tool_use_id"] = "small"
	tool["tool_response"] = "new"
	if len(invoke(t, store, tool)) != 0 {
		t.Fatal("deferred bytes requested maintenance")
	}
	tool["tool_use_id"] = "unicode"
	tool["tool_response"] = strings.Repeat("界", 14000)
	if len(invoke(t, store, tool)) == 0 {
		t.Fatal("pending UTF-8 bytes did not request maintenance")
	}
	if len(invoke(t, store, tool)) != 0 {
		t.Fatal("duplicate reminder for cursor")
	}
	cp = capturedCheckpoint(t, l)
	cp.Acknowledge = cp.Acknowledge[:1]
	if _, err = l.ApplyV2(cp); err != nil {
		t.Fatal(err)
	}
	tool["tool_use_id"] = "after-prefix"
	tool["tool_response"] = "new"
	if len(invoke(t, store, tool)) == 0 {
		t.Fatal("new resolved cursor did not permit reminder")
	}
}

func TestStopRecursionDoesNotStealNewRoot(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	promptRoot(t, store, "old", strings.Repeat("work", 11000))
	first := stopRoot(t, store, "old", "old final")
	reason, ok := first["reason"].(string)
	if !ok {
		t.Fatal("missing initial owned continuation")
	}
	promptRoot(t, store, "owned-synthetic", reason)
	owned := rootEvent("Stop", "owned-synthetic")
	owned["stop_hook_active"] = true
	owned["last_assistant_message"] = "Owned continuation final"
	for range 2 {
		if len(invoke(t, store, owned)) != 0 {
			t.Fatal("owned synthetic continued")
		}
		count, err := l.State("root_completed_ordinal")
		if err != nil || count != "1" {
			t.Fatal("owned synthetic counted as root", count, err)
		}
	}
	if _, err := l.ApplyV2(capturedCheckpoint(t, l)); err != nil {
		t.Fatal(err)
	}
	promptRoot(t, store, "new", "New real user request")
	if _, err := l.ApplyV2(capturedCheckpoint(t, l)); err != nil {
		t.Fatal(err)
	}
	e := rootEvent("Stop", "new")
	e["stop_hook_active"] = true
	e["last_assistant_message"] = "Final from another hook continuation"
	for range 2 {
		if len(invoke(t, store, e)) != 0 {
			t.Fatal("active stop continued")
		}
		count, err := l.State("root_completed_ordinal")
		if err != nil || count != "2" {
			t.Fatal("known guarded real root must complete once", count, err)
		}
		status, err := l.Status()
		if err != nil || status.SourceCount != 5 || status.OldestPendingAgeTurns != 0 {
			t.Fatal("guarded final capture/origin", status, err)
		}
	}
	ordinal, err := l.CompleteRootTurn("new")
	if err != nil || ordinal != 2 {
		t.Fatal("wrong real root completed", ordinal, err)
	}
	stopRoot(t, store, "boundary", "")
	status, err := l.Status()
	if err != nil || status.OldestPendingAgeTurns != 1 {
		t.Fatal("stale owned prompt stole new final's origin", status, err)
	}
}

func TestStopBacklogAgeGuardedRealCompletions(t *testing.T) {
	store := t.TempDir()
	l := open(t, store)
	for i := 0; i < 4; i++ {
		root := fmt.Sprintf("guarded-%d", i)
		promptRoot(t, store, root, "Routine real work")
		e := rootEvent("Stop", root)
		e["stop_hook_active"] = true
		e["last_assistant_message"] = "Real final"
		for range 2 {
			if len(invoke(t, store, e)) != 0 {
				t.Fatal("already active Stop requested continuation")
			}
			count, err := l.State("root_completed_ordinal")
			if err != nil || count != fmt.Sprint(i+1) {
				t.Fatal("guarded real completion missing or duplicated", count, err)
			}
			status, err := l.Status()
			if err != nil || status.OldestPendingAgeTurns != int64(i) {
				t.Fatal("guarded turns under-aged debt", status, err)
			}
		}
	}
	promptRoot(t, store, "unguarded", "Continue real work")
	if stopRoot(t, store, "unguarded", "Done")["decision"] != "block" {
		t.Fatal("aged real debt did not request bounded maintenance")
	}
}
