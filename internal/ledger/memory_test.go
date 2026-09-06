package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func openTest(t *testing.T, store, session string) *Ledger {
	t.Helper()
	l, err := Open(context.Background(), store, session)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}
func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func ptr(n int64) *int64 { return &n }
func observe(t *testing.T, l *Ledger, text string) (int64, string, string) {
	t.Helper()
	sid, err := l.Capture("user", text, text)
	check(t, err)
	receipt, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: pendingIDs(t, l), Observations: []ObservationV2{{Text: text, Importance: "high", EvidenceIDs: []string{evidenceID(t, l, sid)}}}}))
	check(t, err)
	return receipt.Through, sid, receipt.Observations[0]
}
func TestLegacyRustStoreRefused(t *testing.T) {
	data, err := os.ReadFile("testdata/rust-schema-1.json")
	check(t, err)
	var fixture struct {
		Session       string
		Directory     string
		SourceID      string `json:"source_id"`
		SourceText    string `json:"source_text"`
		ObservationID string `json:"observation_id"`
		Observation   Observation
		ReflectionID  string `json:"reflection_id"`
	}
	check(t, json.Unmarshal(data, &fixture))
	store := t.TempDir()
	dir := filepath.Join(store, fixture.Directory)
	check(t, os.Mkdir(dir, 0700))
	db, err := sql.Open("sqlite", filepath.Join(dir, "memory.sqlite3"))
	check(t, err)
	schema, err := os.ReadFile("testdata/rust-schema-1.sql")
	check(t, err)
	_, err = db.Exec(string(schema))
	check(t, err)
	check(t, db.Close())
	before, err := os.ReadFile(filepath.Join(dir, "memory.sqlite3"))
	check(t, err)
	if l, err := Open(context.Background(), store, fixture.Session); err == nil {
		l.Close()
		t.Fatal("accepted unsupported legacy schema")
	} else if !strings.Contains(err.Error(), "unsupported ledger version") {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "memory.sqlite3"))
	check(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("legacy store changed")
	}
}

func TestCheckpointRollbackIdempotencyAndStaleCursor(t *testing.T) {
	l := openTest(t, t.TempDir(), "atomic")
	captured, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Keep the API stable.", Key: "one"})
	check(t, err)
	cp := checkpointNow(t, l, CheckpointV2{Acknowledge: []string{captured.FirstUnitID}, Observations: []ObservationV2{{Text: "Valid fact.", EvidenceIDs: []string{captured.FirstUnitID}}, {Text: "Unsupported.", EvidenceIDs: []string{"e-missing"}}}})
	before := checkpointSnapshot(t, l)
	if _, err = l.ApplyV2(cp); err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("invalid support mutated checkpoint")
	}
	cp.Observations = cp.Observations[:1]
	one, err := l.ApplyV2(cp)
	check(t, err)
	two, err := l.ApplyV2(cp)
	check(t, err)
	if !reflect.DeepEqual(one, two) {
		t.Fatal("checkpoint not idempotent")
	}
	for _, bad := range []CheckpointV2{{ExpectedThrough: 0}, {ExpectedThrough: 99}, {ExpectedThrough: one.Through, ExpectedRevision: 0}} {
		if _, err = l.ApplyV2(bad); err == nil {
			t.Fatal("accepted stale or future cursor/revision")
		}
	}
}
func TestRetirementAndRecallSurviveReopen(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "retire")
	_, sid, oid := observe(t, l, "Use Rust.")
	_, _, newID := observe(t, l, "User chose Go, replacing Rust.")
	_, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: oid, Reason: "Explicit correction", ReplacementIDs: []string{newID}}}}))
	check(t, err)
	reflected, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Reflections: []Reflection{{Text: "Use Go for this CLI.", ObservationIDs: []string{newID}}}}))
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: newID, Reason: "Retained in reflection", ReplacementIDs: reflected.Reflections}}}))
	check(t, err)
	check(t, l.Close())
	l = openTest(t, store, "retire")
	view, err := l.View()
	check(t, err)
	if strings.Contains(view, "Use Rust.") {
		t.Fatal("retired memory injected")
	}
	old := drainRecall(t, l, oid)
	if old.headers[oid].Active || old.sources[sid] != "Use Rust." {
		t.Fatal("retirement erased evidence")
	}
	r := drainRecall(t, l, reflected.Reflections[0])
	if len(r.headers) != 2 || len(r.sources) != 1 {
		t.Fatal("reflection provenance lost")
	}
}
func TestRejectInvalidSupportAndText(t *testing.T) {
	l := openTest(t, t.TempDir(), "validation")
	_, _, old := observe(t, l, "First decision.")
	_, sid, newer := observe(t, l, "Second decision.")
	reflection, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Reflections: []Reflection{{Text: "Second conclusion.", ObservationIDs: []string{newer}}}}))
	check(t, err)
	cases := []CheckpointV2{
		{Observations: []ObservationV2{{Text: "Unsupported", EvidenceIDs: []string{}}}},
		{Observations: []ObservationV2{{Text: " ", EvidenceIDs: []string{evidenceID(t, l, sid)}}}},
		{Observations: []ObservationV2{{Text: strings.Repeat("a", 2001), EvidenceIDs: []string{evidenceID(t, l, sid)}}}},
		{Observations: []ObservationV2{{Text: "Bad importance", Importance: "urgent", EvidenceIDs: []string{evidenceID(t, l, sid)}}}},
		{Reflections: []Reflection{{Text: "Wrong support kind", ObservationIDs: reflection.Reflections}}},
		{Retire: []Retirement{{ID: old, Reason: "Not covered", ReplacementIDs: reflection.Reflections}}},
		{Retire: []Retirement{{ID: newer, Reason: "Not newer", ReplacementIDs: []string{old}}}},
		{Retire: []Retirement{{ID: old, Reason: "No support"}}},
	}
	for i, cp := range cases {
		cp = checkpointNow(t, l, cp)
		if _, err = l.ApplyV2(cp); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if _, err = l.Capture("other", "Text", "key"); err == nil {
		t.Fatal("accepted invalid role")
	}
}
func TestForkAndConcurrentSessions(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "parent")
	_, _, oid := observe(t, l, "Shared evidence.")
	check(t, l.SetState("stop_turn", "already-stopped"))
	other := openTest(t, store, "other")
	if _, err := other.ReadRecall(oid, ""); err == nil {
		t.Fatal("session leak")
	}
	if _, err := l.Fork("other"); err == nil {
		t.Fatal("overwrote destination")
	}
	_, err := l.Fork("child")
	check(t, err)
	child := openTest(t, store, "child")
	_, err = child.ReadRecall(oid, "")
	check(t, err)
	state, err := child.State("stop_turn")
	check(t, err)
	if state != "" {
		t.Fatal("fork copied transient state")
	}
	observe(t, child, "Child-only decision.")
	entries, err := l.ReadEntries("")
	check(t, err)
	headers := 0
	for _, item := range entries.Page.Items {
		if item.Header != nil {
			headers++
		}
	}
	if headers != 1 {
		t.Fatal("fork shares writes")
	}
	traversal := openTest(t, store, "../../escape")
	if !strings.HasPrefix(traversal.Path, filepath.Dir(filepath.Dir(l.Path))+string(os.PathSeparator)) {
		t.Fatal("session escaped store")
	}
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			writer, err := Open(context.Background(), store, "parent")
			if err == nil {
				defer writer.Close()
				_, err = writer.Capture("tool", fmt.Sprintf("Evidence %d", i), fmt.Sprint(i))
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		check(t, err)
	}
	pending, err := l.ReadPending("")
	check(t, err)
	if len(pending.Page.Items) != 8 {
		t.Fatal("concurrent evidence lost")
	}
}
func TestUnicodeBoundsViewAndRedaction(t *testing.T) {
	l := openTest(t, t.TempDir(), "bounds")
	for i := range 5 {
		_, err := l.Capture("tool", strings.Repeat("🙂", 30000), fmt.Sprint(i))
		check(t, err)
	}
	count := 0
	for {
		pending, err := l.ReadPending("")
		check(t, err)
		if len(pending.Page.Items) == 0 {
			break
		}
		id := pending.Page.Items[0].SourceID
		if drainRecall(t, l, id).sources[id] != strings.Repeat("🙂", 30000) {
			t.Fatal("source was not retained completely")
		}
		_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{SourceID: id, Reason: "Routine retained log"}}}))
		check(t, err)
		count++
	}
	if count != 5 {
		t.Fatal("backlog did not drain")
	}
	for i := range 30 {
		observe(t, l, fmt.Sprintf("Event %d %s", i, strings.Repeat("detail ", 100)))
	}
	sid, err := l.Capture("user", "Never repeat the completed migration.", "critical")
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: pendingIDs(t, l), Observations: []ObservationV2{{Text: "Never repeat the completed migration.", Importance: "critical", EvidenceIDs: []string{evidenceID(t, l, sid)}}}}))
	check(t, err)
	view, err := l.View()
	check(t, err)
	if utf8.RuneCountInString(view) > ViewLimit || !strings.Contains(view, "Never repeat") || strings.Contains(view, "(0 active entries omitted") {
		t.Fatal("view limits/priority failed")
	}
	secret := "Bearer abc.def-secret sk-12345678901234567890 ghp_1234567890123456789012345\n-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"
	if strings.Contains(Redact(secret), "secret") || strings.Contains(Redact(secret), "1234567890") {
		t.Fatal("credential redaction failed")
	}
}
func TestRecallOrdersEvidence(t *testing.T) {
	l := openTest(t, t.TempDir(), "order")
	ids := []string{}
	for _, text := range []string{"First", "Second", "Third"} {
		id, err := l.Capture("user", text, text)
		check(t, err)
		ids = append(ids, evidenceID(t, l, id))
	}
	receipt, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: pendingIDs(t, l), Observations: []ObservationV2{{Text: "Combined.", EvidenceIDs: ids}}}))
	check(t, err)
	recall := drainRecall(t, l, receipt.Observations[0])
	for i, e := range recall.evidence {
		if e.Seq != int64(i+1) {
			t.Fatal("evidence reordered")
		}
	}
	if len(recall.headers) != 1 {
		t.Fatal("duplicated observation")
	}
}
