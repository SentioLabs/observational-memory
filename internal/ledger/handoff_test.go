package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// handoffRows names every persisted column used in a logical comparison. It
// deliberately excludes derived FTS shadow tables and physical SQLite layout.
func handoffRows(t *testing.T, l *Ledger, query string) []string {
	t.Helper()
	rows, err := l.db.Query(query)
	check(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	check(t, err)
	result := []string{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		check(t, rows.Scan(pointers...))
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				values[i] = string(b)
			}
		}
		body, err := json.Marshal(values)
		check(t, err)
		result = append(result, string(body))
	}
	check(t, rows.Err())
	return result
}

type handoffSnapshot struct {
	Sources, Units, Entries, State, Meta, Roots, Receipts, SourceIndex, EntryIndex []string
}

func readSnapshotForTest(t *testing.T, l *Ledger) handoffSnapshot {
	t.Helper()
	return handoffSnapshot{
		Sources:     handoffRows(t, l, `SELECT seq,id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete FROM sources ORDER BY seq`),
		Units:       handoffRows(t, l, `SELECT seq,id,source_id,start_byte,end_byte,review_state,deferral_reason FROM evidence_units ORDER BY seq`),
		Entries:     handoffRows(t, l, `SELECT seq,id,body,active,retirement,effective_priority FROM entries ORDER BY seq`),
		State:       handoffRows(t, l, `SELECT singleton,body,revision,updated_at,local_origin_ordinal FROM working_state`),
		Meta:        handoffRows(t, l, `SELECT key,value FROM meta ORDER BY key`),
		Roots:       handoffRows(t, l, `SELECT id,completed_ordinal,continuation_claimed,continuation_prompt FROM root_turns ORDER BY id`),
		Receipts:    handoffRows(t, l, `SELECT digest,body FROM checkpoint_receipts ORDER BY digest`),
		SourceIndex: handoffRows(t, l, `SELECT rowid,text FROM source_fts ORDER BY rowid`),
		EntryIndex:  handoffRows(t, l, `SELECT rowid,text FROM entry_fts ORDER BY rowid`),
	}
}

func seedHandoffV2(t *testing.T, l *Ledger) CheckpointV2 {
	t.Helper()
	_, err := l.db.Exec(`INSERT INTO root_turns(id,completed_ordinal,continuation_claimed,continuation_prompt) VALUES('old-root',7,1,'old continuation'); UPDATE meta SET value='12' WHERE key='root_completed_ordinal'`)
	check(t, err)
	a, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "continuity old approved constraint", Key: "same-key", RootTurnID: "old-root", OriginCompletionOrdinal: ptr(7)})
	check(t, err)
	b, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("continuity deferred 界🙂\n", 300), Key: "log", RootTurnID: "old-root", OriginCompletionOrdinal: ptr(7), SourceIncomplete: true})
	check(t, err)
	cp := checkpointNow(t, l, CheckpointV2{Acknowledge: []string{a.FirstUnitID}, DeferSources: []SourceDeferral{{SourceID: b.SourceID, Reason: "retained diagnostic log"}}, Observations: []ObservationV2{{Text: "continuity old constraint", Importance: "critical", EvidenceIDs: []string{a.FirstUnitID}}, {Text: "continuity replacement constraint", EvidenceIDs: []string{a.FirstUnitID}}}, WorkingState: &WorkingState{Objective: &WorkingFact{Text: "Continue continuity implementation", EvidenceIDs: []string{a.FirstUnitID}}, Next: []WorkingFact{{Text: "Verify next step", EvidenceIDs: []string{b.FirstUnitID}}}}})
	receipt, err := l.ApplyV2(cp)
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{ReviewDeferred: []string{b.FirstUnitID}, Retire: []Retirement{{ID: receipt.Observations[0], Reason: "corrected", ReplacementIDs: []string{receipt.Observations[1]}}}}))
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Reflections: []Reflection{{Text: "continuity consolidated constraint", ObservationIDs: []string{receipt.Observations[1]}}}}))
	check(t, err)
	_, err = l.CaptureV2(CaptureInput{Kind: "assistant", Text: strings.Repeat("continuity pending ", 700), Key: "pending", RootTurnID: "old-root", OriginCompletionOrdinal: ptr(7)})
	check(t, err)
	for _, key := range []string{"stop_turn", "stop_prompt", "notified_cursor", "prompt_id"} {
		check(t, l.SetState(key, "old-hook"))
	}
	check(t, l.Pause(true))
	return cp
}

func assertHandoffCopied(t *testing.T, source, target *Ledger, localIDs []string) {
	t.Helper()
	for _, query := range []string{
		`SELECT id,body,origin_session,origin_key,root_turn_id,origin_completion_ordinal,source_incomplete FROM sources ORDER BY seq`,
		`SELECT seq,id,source_id,start_byte,end_byte,review_state,deferral_reason FROM evidence_units ORDER BY seq`,
		`SELECT seq,id,body,active,retirement,effective_priority FROM entries ORDER BY seq`,
		`SELECT body,updated_at FROM working_state`,
	} {
		want, got := handoffRows(t, source, query), handoffRows(t, target, query)
		if len(got) < len(want) || !reflect.DeepEqual(want, got[:len(want)]) {
			t.Fatalf("snapshot content changed for %s\nwant %v\ngot %v", query, want, got)
		}
	}
	for _, id := range localIDs {
		for _, unit := range readTestUnits(t, target, id) {
			if unit.ReviewState != ReviewPending {
				t.Fatal("local prompt was treated as reviewed")
			}
		}
	}
	for _, pair := range [][2]string{{"sources", "source_fts"}, {"entries", "entry_fts"}} {
		base := handoffRows(t, target, `SELECT seq,json_extract(body,'$.text') FROM `+pair[0]+` ORDER BY seq`)
		index := handoffRows(t, target, `SELECT rowid,text FROM `+pair[1]+` ORDER BY rowid`)
		if !reflect.DeepEqual(base, index) {
			t.Fatalf("partial or duplicate %s index", pair[1])
		}
	}
	for _, options := range []SearchOptions{{Sources: true}, {IncludeRetired: true}, {}} {
		page, err := target.ReadSearch("continuity", options)
		check(t, err)
		if len(page.Page.Items) == 0 {
			t.Fatal("copied search index returned no matches")
		}
	}
	status, err := target.Status()
	check(t, err)
	original, err := source.Status()
	check(t, err)
	if status.Through != original.Through || status.Coverage.Reviewed != original.Coverage.Reviewed || status.Coverage.Deferred != original.Coverage.Deferred {
		t.Fatal("historical coverage changed")
	}
	if status.WorkingState == nil || status.WorkingState.AgeRootTurns != 0 || status.OldestPendingAgeTurns != 0 {
		t.Fatalf("foreign root ordinals aged destination: %+v", status)
	}
	if len(handoffRows(t, target, `SELECT digest,body FROM checkpoint_receipts`)) != 0 || len(handoffRows(t, target, `SELECT id FROM root_turns`)) != 0 {
		t.Fatal("source retry or root state survived")
	}
	for _, key := range []string{"stop_turn", "stop_prompt", "notified_cursor"} {
		value, err := target.State(key)
		check(t, err)
		if value != "" {
			t.Fatalf("stale %s", key)
		}
	}
	sourceEpoch, err := source.State("pagination_epoch")
	check(t, err)
	targetEpoch, err := target.State("pagination_epoch")
	check(t, err)
	if sourceEpoch == targetEpoch {
		t.Fatal("copied source pagination epoch")
	}
	var anchored int
	check(t, target.db.QueryRow(`SELECT COUNT(*) FROM sources WHERE local_origin_ordinal IS NOT NULL AND EXISTS(SELECT 1 FROM evidence_units WHERE source_id=sources.id AND review_state='pending')`).Scan(&anchored))
	if anchored != 0 {
		t.Fatal("pending source kept a foreign local age anchor")
	}
}

func assertHandoffLocalAge(t *testing.T, target *Ledger) {
	t.Helper()
	// Exercise the persisted root-boundary contract; actual cadence methods are
	// owned by the cadence task. Deferred-only logs remain outside pending age.
	_, err := target.db.Exec(`UPDATE sources SET local_origin_ordinal=1 WHERE local_origin_ordinal IS NULL AND EXISTS(SELECT 1 FROM evidence_units WHERE source_id=sources.id AND review_state='pending'); UPDATE meta SET value='1' WHERE key='root_completed_ordinal'`)
	check(t, err)
	status, err := target.Status()
	check(t, err)
	if status.OldestPendingAgeTurns != 0 {
		t.Fatal("first local boundary fabricated backlog age")
	}
	_, err = target.db.Exec(`UPDATE meta SET value='4' WHERE key='root_completed_ordinal'`)
	check(t, err)
	status, err = target.Status()
	check(t, err)
	if status.OldestPendingAgeTurns != 3 {
		t.Fatal("pending age did not follow local boundaries")
	}
	// Only deferred history remains after the pending prefix is reviewed.
	for len(pendingIDs(t, target)) > 0 {
		ids := pendingIDs(t, target)
		if len(ids) > MaxCheckpointItems {
			ids = ids[:MaxCheckpointItems]
		}
		_, err = target.ApplyV2(checkpointNow(t, target, CheckpointV2{Acknowledge: ids}))
		check(t, err)
	}
	status, err = target.Status()
	check(t, err)
	if status.OldestPendingAgeTurns != 0 || status.Coverage.Deferred.Units == 0 {
		t.Fatal("deferred gap became maintenance debt or disappeared")
	}
}

func TestForkV2SnapshotContinuity(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	retry := seedHandoffV2(t, source)
	before := readSnapshotForTest(t, source)
	status, err := source.Fork("child")
	check(t, err)
	if !status.Paused || status.Session != "child" {
		t.Fatal("fork pause or identity changed")
	}
	child := openTest(t, source.store, "child")
	assertHandoffCopied(t, source, child, nil)
	assertHandoffLocalAge(t, child)
	if _, err = child.ApplyV2(retry); err == nil {
		t.Fatal("source retry replayed in fork")
	}
	if !reflect.DeepEqual(before, readSnapshotForTest(t, source)) {
		t.Fatal("fork modified source")
	}
	childBefore := readSnapshotForTest(t, child)
	_, err = source.Capture("tool", "later original write", "later")
	check(t, err)
	if !reflect.DeepEqual(childBefore, readSnapshotForTest(t, child)) {
		t.Fatal("fork shares later source writes")
	}
	info, err := os.Stat(child.Path)
	check(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("fork mode %o", info.Mode().Perm())
	}
}

func TestImportV2SnapshotAndLocalPrompts(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	retry := seedHandoffV2(t, source)
	before := readSnapshotForTest(t, source)
	target := openTest(t, t.TempDir(), "destination")
	_, err := target.db.Exec(`INSERT INTO root_turns(id,completed_ordinal) VALUES('local-root',5); UPDATE meta SET value='8' WHERE key='root_completed_ordinal'`)
	check(t, err)
	var localIDs []string
	for _, input := range []CaptureInput{{Kind: "user", Text: "continuity old approved constraint", Key: "same-key", RootTurnID: "local-root"}, {Kind: "tool", Text: "continuity local log", Key: "local"}} {
		receipt, err := target.CaptureV2(input)
		check(t, err)
		localIDs = append(localIDs, receipt.SourceID)
	}
	check(t, target.SetState("prompt_id", localIDs[0]))
	check(t, target.SetState("stop_turn", "local old guard"))
	old, err := target.Status()
	check(t, err)
	status, err := target.Import(source.store, source.session)
	check(t, err)
	if status.Paused || status.Session != target.session || status.Revision != old.Revision+1 {
		t.Fatalf("destination policy/revision changed: %+v", status)
	}
	prompt, err := target.State("prompt_id")
	check(t, err)
	if prompt != localIDs[0] {
		t.Fatal("lost current prompt identity")
	}
	assertHandoffCopied(t, source, target, localIDs)
	assertHandoffLocalAge(t, target)
	if _, err = target.ApplyV2(retry); err == nil {
		t.Fatal("source retry replayed in import")
	}
	if !reflect.DeepEqual(before, readSnapshotForTest(t, source)) {
		t.Fatal("import modified source")
	}
}

func TestImportV2InvalidatesCollidingTokens(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	target := openTest(t, t.TempDir(), "target")
	for _, l := range []*Ledger{source, target} {
		for i := range 35 {
			_, err := l.Capture("tool", fmt.Sprintf("continuity %d %s", i, strings.Repeat("details ", 100)), fmt.Sprint(i))
			check(t, err)
		}
		_, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: []string{}}))
		check(t, err)
	}
	old, err := target.Status()
	check(t, err)
	pending, err := target.ReadPending("")
	check(t, err)
	search, err := target.ReadSearch("continuity", SearchOptions{Sources: true})
	check(t, err)
	if pending.Page.NextCursor == "" || search.Page.NextCursor == "" {
		t.Fatal("fixture needs multiple pages")
	}
	_, err = target.Import(source.store, source.session)
	check(t, err)
	if _, err = target.ReadPending(pending.Page.NextCursor); err == nil {
		t.Fatal("pre-import pending token accepted")
	}
	if _, err = target.ReadSearch("continuity", SearchOptions{Sources: true, Cursor: search.Page.NextCursor}); err == nil {
		t.Fatal("pre-import search token accepted")
	}
	if _, err = target.ApplyV2(CheckpointV2{ExpectedThrough: old.Through, ExpectedRevision: old.Revision, Acknowledge: []string{}}); err == nil {
		t.Fatal("pre-import checkpoint accepted")
	}
}

func TestImportV2RejectsPreparedDestination(t *testing.T) {
	for _, kind := range []string{"entry", "reviewed", "deferred", "state", "imported"} {
		t.Run(kind, func(t *testing.T) {
			source := openTest(t, t.TempDir(), "source")
			target := openTest(t, t.TempDir(), "target")
			receipt, err := target.CaptureV2(CaptureInput{Kind: "tool", Text: "local evidence", Key: "local"})
			check(t, err)
			switch kind {
			case "entry":
				_, err = target.ApplyV2(checkpointNow(t, target, CheckpointV2{Observations: []ObservationV2{{Text: "prepared", EvidenceIDs: []string{receipt.FirstUnitID}}}}))
			case "reviewed":
				_, err = target.db.Exec(`UPDATE evidence_units SET review_state='reviewed'`) // Also reject inconsistent zero-cursor progress.
			case "deferred":
				_, err = target.db.Exec(`UPDATE evidence_units SET review_state='deferred',deferral_reason='reason'`)
			case "state":
				_, err = target.ApplyV2(checkpointNow(t, target, CheckpointV2{WorkingState: &WorkingState{}}))
			case "imported":
				_, err = target.Import(source.store, source.session)
			}
			check(t, err)
			before := readSnapshotForTest(t, target)
			if _, err = target.Import(source.store, source.session); err == nil {
				t.Fatal("accepted prepared " + kind)
			}
			if !reflect.DeepEqual(before, readSnapshotForTest(t, target)) {
				t.Fatal("rejection changed destination")
			}
		})
	}
}

func TestForkV2ConcurrentPublication(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	seedHandoffV2(t, source)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := source.Fork("child"); results <- err })
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected one fork, got %d", succeeded)
	}
	child := openTest(t, source.store, "child")
	assertHandoffCopied(t, source, child, nil)
}

// A per-operation context supplies deterministic boundary injection without a
// global hook or timing-dependent cancellation races.
type handoffTestContext struct {
	context.Context
	boundary func(string)
}

func (c handoffTestContext) handoffBoundary(stage string) { c.boundary(stage) }

func assertNoSnapshots(t *testing.T, store string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(store, ".snapshot-*"))
	check(t, err)
	if len(files) != 0 {
		t.Fatalf("private snapshots leaked: %v", files)
	}
}
func TestHandoffCancellation(t *testing.T) {
	for _, operation := range []string{"fork", "import"} {
		stages := []string{"backup", "metadata"}
		if operation == "fork" {
			stages = append(stages, "publication")
		} else {
			stages = append(stages, "import")
		}
		for _, stage := range stages {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				source := openTest(t, t.TempDir(), "source")
				seedHandoffV2(t, source)
				target := openTest(t, t.TempDir(), "target")
				_, err := target.Capture("user", "keep current prompt", "local")
				check(t, err)
				sourceBefore, targetBefore := readSnapshotForTest(t, source), readSnapshotForTest(t, target)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				called := false
				hooked := handoffTestContext{Context: ctx, boundary: func(at string) {
					if at == stage {
						called = true
						cancel()
					}
				}}
				store, session := source.store, source.session
				if operation == "import" {
					store, session = target.store, target.session
				}
				active, err := Open(hooked, store, session)
				check(t, err)
				defer active.Close()
				var status Status
				if operation == "fork" {
					status, err = active.Fork("child")
				} else {
					status, err = active.Import(source.store, source.session)
				}
				if !called || !errors.Is(err, context.Canceled) || status.Session != "" {
					t.Fatalf("cancellation boundary was not atomic: called=%v status=%+v err=%v", called, status, err)
				}
				if !reflect.DeepEqual(sourceBefore, readSnapshotForTest(t, source)) || !reflect.DeepEqual(targetBefore, readSnapshotForTest(t, target)) {
					t.Fatal("cancellation changed source or destination")
				}
				if operation == "fork" {
					name, e := identity("session-", "child")
					check(t, e)
					if _, e = os.Stat(filepath.Join(source.store, name)); !errors.Is(e, os.ErrNotExist) {
						t.Fatal("canceled fork left destination")
					}
				}
				assertNoSnapshots(t, source.store)
				assertNoSnapshots(t, target.store)
			})
		}
	}
}

func TestHandoffCancellationPreservesCompetingStartup(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	seedHandoffV2(t, source)
	var competing *Ledger
	hooked := handoffTestContext{Context: context.Background(), boundary: func(stage string) {
		if stage != "publication" {
			return
		}
		var err error
		competing, err = Open(context.Background(), source.store, "child")
		check(t, err)
		_, err = competing.Capture("user", "concurrent startup owns this file", "current")
		check(t, err)
	}}
	active, err := Open(hooked, source.store, source.session)
	check(t, err)
	defer active.Close()
	if _, err = active.Fork("child"); err == nil {
		t.Fatal("fork replaced concurrent startup")
	}
	if competing == nil {
		t.Fatal("publication boundary not exercised")
	}
	defer competing.Close()
	status, err := competing.Status()
	check(t, err)
	if status.SourceCount != 1 || status.Active.Observations != 0 || status.ImportedFrom != nil {
		t.Fatal("failed cleanup touched competing file")
	}
	assertNoSnapshots(t, source.store)
}

func TestHandoffCancellationSQLRollback(t *testing.T) {
	for _, operation := range []string{"fork", "import"} {
		t.Run(operation, func(t *testing.T) {
			source := openTest(t, t.TempDir(), "source")
			seedHandoffV2(t, source)
			target := openTest(t, t.TempDir(), "target")
			_, err := target.Capture("user", "keep local prompt", "local")
			check(t, err)
			if operation == "fork" {
				_, err = source.db.Exec(`CREATE TRIGGER fail_handoff BEFORE UPDATE ON working_state BEGIN SELECT RAISE(ABORT,'injected metadata failure'); END`)
			} else {
				_, err = target.db.Exec(`CREATE TRIGGER fail_handoff BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT,'injected import failure'); END`)
			}
			check(t, err)
			sourceBefore, targetBefore := readSnapshotForTest(t, source), readSnapshotForTest(t, target)
			if operation == "fork" {
				_, err = source.Fork("child")
			} else {
				_, err = target.Import(source.store, source.session)
			}
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("expected injected failure, got %v", err)
			}
			if !reflect.DeepEqual(sourceBefore, readSnapshotForTest(t, source)) || !reflect.DeepEqual(targetBefore, readSnapshotForTest(t, target)) {
				t.Fatal("failed SQL changed snapshot")
			}
			if operation == "fork" {
				name, e := identity("session-", "child")
				check(t, e)
				if _, e = os.Stat(filepath.Join(source.store, name)); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("failed fork left destination")
				}
			}
			assertNoSnapshots(t, source.store)
		})
	}
}

func TestHandoffCancellationDuringBackup(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	_, err := source.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("continuity backup pages ", 30000), Key: "large"})
	check(t, err)
	before := readSnapshotForTest(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := 0
	active, err := Open(handoffTestContext{Context: ctx, boundary: func(stage string) {
		if stage == "backup" {
			steps++
			if steps == 2 {
				cancel()
			}
		}
	}}, source.store, source.session)
	check(t, err)
	defer active.Close()
	if _, err = active.Fork("child"); !errors.Is(err, context.Canceled) || steps != 2 {
		t.Fatalf("did not cancel after backup progress: steps=%d err=%v", steps, err)
	}
	if !reflect.DeepEqual(before, readSnapshotForTest(t, source)) {
		t.Fatal("partial backup changed source")
	}
	name, err := identity("session-", "child")
	check(t, err)
	if _, err = os.Stat(filepath.Join(source.store, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial backup published a fork")
	}
	assertNoSnapshots(t, source.store)
}

func TestForkV2SnapshotBeforePublicationIsIsolated(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	seedHandoffV2(t, source)
	before := readSnapshotForTest(t, source)
	changed := false
	active, err := Open(handoffTestContext{Context: context.Background(), boundary: func(stage string) {
		if stage != "metadata" {
			return
		}
		changed = true
		_, err := source.Capture("tool", "later source mutation is excluded from snapshot", "later")
		check(t, err)
	}}, source.store, source.session)
	check(t, err)
	defer active.Close()
	_, err = active.Fork("child")
	check(t, err)
	if !changed {
		t.Fatal("fixture did not mutate source after backup")
	}
	child := openTest(t, source.store, "child")
	after := readSnapshotForTest(t, child)
	if !reflect.DeepEqual(before.Units, after.Units) || !reflect.DeepEqual(before.Entries, after.Entries) || !reflect.DeepEqual(before.SourceIndex, after.SourceIndex) || !reflect.DeepEqual(before.EntryIndex, after.EntryIndex) {
		t.Fatal("snapshot included writes after backup")
	}
	if reflect.DeepEqual(before.Units, readSnapshotForTest(t, source).Units) {
		t.Fatal("source mutation did not persist")
	}
	assertNoSnapshots(t, source.store)
}

func TestImportV2CancellationAfterCommitReportsSuccess(t *testing.T) {
	source := openTest(t, t.TempDir(), "source")
	seedHandoffV2(t, source)
	target := openTest(t, t.TempDir(), "target")
	_, err := target.Capture("user", "current request remains pending", "local")
	check(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	committed := false
	active, err := Open(handoffTestContext{Context: ctx, boundary: func(stage string) {
		if stage == "committed" {
			committed = true
			cancel()
		}
	}}, target.store, target.session)
	check(t, err)
	defer active.Close()
	status, err := active.Import(source.store, source.session)
	check(t, err)
	if !committed || status.Session != target.session || status.ImportedFrom == nil || status.SourceCount != 4 {
		t.Fatalf("committed import reported cancellation or incomplete status: committed=%v status=%+v", committed, status)
	}
	actual, err := target.Status()
	check(t, err)
	if !reflect.DeepEqual(status, actual) {
		t.Fatal("reported committed status differs from durable destination")
	}
	assertNoSnapshots(t, source.store)
}
