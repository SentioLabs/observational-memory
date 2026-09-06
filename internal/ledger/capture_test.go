package ledger

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func readTestUnits(t *testing.T, l *Ledger, sourceID string) []EvidenceUnit {
	t.Helper()
	s, err := source(l.ctx, l.db, sourceID)
	check(t, err)
	rows, err := l.db.QueryContext(l.ctx, `SELECT seq,id,start_byte,end_byte,review_state,deferral_reason FROM evidence_units WHERE source_id=? ORDER BY seq`, sourceID)
	check(t, err)
	defer rows.Close()
	units := []EvidenceUnit{}
	var end int64
	for rows.Next() {
		u := EvidenceUnit{SourceID: sourceID, Kind: s.Kind, Timestamp: s.Timestamp}
		check(t, rows.Scan(&u.Seq, &u.ID, &u.StartByte, &u.EndByte, &u.ReviewState, &u.DeferralReason))
		if u.StartByte != end || u.EndByte <= end || u.EndByte > int64(len(s.Text)) {
			t.Fatalf("invalid range %+v", u)
		}
		u.Text = s.Text[u.StartByte:u.EndByte]
		units = append(units, u)
		end = u.EndByte
	}
	check(t, rows.Err())
	if end != int64(len(s.Text)) {
		t.Fatal("incomplete source coverage")
	}
	return units
}

func TestSourceRetentionAndEvidenceSegmentation(t *testing.T) {
	l := openTest(t, t.TempDir(), "retention")
	inputs := []string{
		strings.Repeat("left\n", 3000) + "exact-middle-error: E_PIPE_742" + strings.Repeat("right\n", 3000),
		strings.Repeat("界🙂\x01\\\"\n", 12000),
		"  leading and trailing whitespace must survive  \n",
		" \n\t ",
		"a < b & c > d\u2028\u2029\r\b\f",
		"Bearer abc.def-secret\nremaining text",
	}
	for i, text := range inputs {
		input := CaptureInput{Kind: "tool", Text: text, Key: strconv.Itoa(i)}
		receipt, err := l.CaptureV2(input)
		check(t, err)
		units := readTestUnits(t, l, receipt.SourceID)
		if len(units) != int(receipt.UnitCount) || units[0].ID != receipt.FirstUnitID {
			t.Fatal("incorrect receipt")
		}
		var reconstructed strings.Builder
		for _, u := range units {
			encoded, err := JSON(u.Text)
			check(t, err)
			if len(encoded) > EvidenceTextJSONBytes || !utf8.ValidString(u.Text) {
				t.Fatal("invalid evidence text", u.ID)
			}
			if u.ReviewState != ReviewPending {
				t.Fatal("capture inferred review")
			}
			reconstructed.WriteString(u.Text)
		}
		if reconstructed.String() != Redact(text) {
			t.Fatal("source reconstruction changed bytes")
		}
		again, err := l.CaptureV2(input)
		check(t, err)
		if !reflect.DeepEqual(receipt, again) {
			t.Fatal("capture retry changed receipt")
		}
	}
}

func TestSourceRetentionAtomicFailureAndValidation(t *testing.T) {
	l := openTest(t, t.TempDir(), "rollback")
	_, err := l.db.Exec(`CREATE TRIGGER fail_units BEFORE INSERT ON evidence_units WHEN NEW.start_byte>0 BEGIN SELECT RAISE(ABORT,'injected unit failure'); END`)
	check(t, err)
	if _, err = l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("x", 5000), Key: "fail"}); err == nil {
		t.Fatal("accepted failing insert")
	}
	var sources, units int
	check(t, l.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&sources))
	check(t, l.db.QueryRow(`SELECT COUNT(*) FROM evidence_units`).Scan(&units))
	if sources != 0 || units != 0 {
		t.Fatal("partial source committed")
	}
	_, err = l.db.Exec(`DROP TRIGGER fail_units`)
	check(t, err)
	for _, in := range []CaptureInput{{Kind: "bad", Text: "x"}, {Kind: "user", Text: ""}, {Kind: "user", Text: string([]byte{255})}, {Kind: "user", Text: strings.Repeat("x", 1000001)}, {Kind: "user", Text: "x", OriginCompletionOrdinal: ptr(-1)}} {
		if _, err = l.CaptureV2(in); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled, err := Open(ctx, l.store, l.session)
	check(t, err)
	defer canceled.Close()
	cancel()
	if _, err = canceled.CaptureV2(CaptureInput{Kind: "user", Text: "canceled"}); err == nil {
		t.Fatal("accepted canceled capture")
	}
	check(t, l.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&sources))
	if sources != 0 {
		t.Fatal("failed capture wrote source")
	}
}

func TestSourceRetentionOriginAndCoverage(t *testing.T) {
	store := t.TempDir()
	a := openTest(t, store, "a")
	b := openTest(t, store, "b")
	input := CaptureInput{Kind: "tool", Text: strings.Repeat("界🙂", 1500), Key: "same", RootTurnID: "root", OriginCompletionOrdinal: ptr(19), SourceIncomplete: true}
	one, err := a.CaptureV2(input)
	check(t, err)
	two, err := b.CaptureV2(input)
	check(t, err)
	if one.SourceID == two.SourceID {
		t.Fatal("source identity is not origin-qualified")
	}
	var origin, key, root string
	var ordinal int64
	var incomplete bool
	var local any
	check(t, a.db.QueryRow(`SELECT origin_session,origin_key,root_turn_id,origin_completion_ordinal,source_incomplete,local_origin_ordinal FROM sources WHERE id=?`, one.SourceID).Scan(&origin, &key, &root, &ordinal, &incomplete, &local))
	if origin != "a" || key != "same" || root != "root" || ordinal != 19 || !incomplete || local != nil {
		t.Fatal("provenance or initial age lost")
	}
	units := readTestUnits(t, a, one.SourceID)
	_, err = a.db.Exec(`UPDATE evidence_units SET review_state='reviewed' WHERE id=?`, units[0].ID)
	check(t, err)
	_, err = a.db.Exec(`UPDATE evidence_units SET review_state='deferred',deferral_reason='large log' WHERE id=?`, units[1].ID)
	check(t, err)
	status, err := a.Status()
	check(t, err)
	if status.SourceCount != 1 || status.UnitCount != one.UnitCount || status.StoredSourceBytes != int64(len(input.Text)) || status.Coverage.Reviewed.Units != 1 || status.Coverage.Deferred.Units != 1 || status.Coverage.Pending.Units != one.UnitCount-2 {
		t.Fatalf("bad coverage: %+v", status)
	}
	if status.Coverage.Pending.Bytes+status.Coverage.Reviewed.Bytes+status.Coverage.Deferred.Bytes != status.StoredSourceBytes {
		t.Fatal("coverage does not partition source bytes")
	}
}

func TestSourceRetentionAcceptedLimitAndConcurrentRetry(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "limit")
	input := CaptureInput{Kind: "tool", Text: strings.Repeat("🙂", 1000000), Key: "max"}
	receipt, err := l.CaptureV2(input)
	check(t, err)
	var text strings.Builder
	for _, unit := range readTestUnits(t, l, receipt.SourceID) {
		text.WriteString(unit.Text)
	}
	if text.String() != input.Text {
		t.Fatal("lost source at accepted character limit")
	}
	other := openTest(t, store, "limit")
	input = CaptureInput{Kind: "user", Text: "same concurrent event", Key: "retry"}
	results := make(chan CaptureReceipt, 2)
	errs := make(chan error, 2)
	for _, writer := range []*Ledger{l, other} {
		go func(writer *Ledger) { receipt, err := writer.CaptureV2(input); results <- receipt; errs <- err }(writer)
	}
	one, two := <-results, <-results
	check(t, <-errs)
	check(t, <-errs)
	if one != two {
		t.Fatal("concurrent retry duplicated queue span")
	}
}

func TestSourceRetentionImmutableOriginAndLocalAge(t *testing.T) {
	l := openTest(t, t.TempDir(), "origins")
	_, err := l.db.Exec(`INSERT INTO root_turns(id,completed_ordinal) VALUES('completed',7); UPDATE meta SET value='9' WHERE key='root_completed_ordinal'`)
	check(t, err)
	receipt, err := l.CaptureV2(CaptureInput{Kind: "assistant", Text: "completion", Key: "key", RootTurnID: "completed", OriginCompletionOrdinal: ptr(40), SourceIncomplete: true})
	check(t, err)
	again, err := l.CaptureV2(CaptureInput{Kind: "assistant", Text: "completion", Key: "key", RootTurnID: "different", OriginCompletionOrdinal: ptr(99)})
	check(t, err)
	if receipt != again {
		t.Fatal("duplicate metadata changed source identity")
	}
	var root string
	var local, origin int64
	var incomplete bool
	check(t, l.db.QueryRow(`SELECT root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete FROM sources WHERE id=?`, receipt.SourceID).Scan(&root, &local, &origin, &incomplete))
	if root != "completed" || local != 7 || origin != 40 || !incomplete {
		t.Fatal("duplicate overwrote immutable origin or local age")
	}
	status, err := l.Status()
	check(t, err)
	if status.OldestPendingAgeTurns != 2 {
		t.Fatalf("bad local age: %+v", status)
	}
	_, err = l.db.Exec(`UPDATE evidence_units SET review_state='deferred'`)
	check(t, err)
	status, err = l.Status()
	check(t, err)
	if status.PendingSources != 0 || status.PendingChars != 0 || status.EstimatedPendingTokens != 0 || status.OldestPendingAgeTurns != 0 {
		t.Fatal("deferred evidence counted as pending")
	}
}

func TestSourceRetentionStatusCountsPendingUnicodeAndNUL(t *testing.T) {
	l := openTest(t, t.TempDir(), "pending-characters")
	receipt, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("\x00\x01界🙂", 4000)})
	check(t, err)
	units := readTestUnits(t, l, receipt.SourceID)
	_, err = l.db.Exec(`UPDATE evidence_units SET review_state='reviewed' WHERE id=?`, units[0].ID)
	check(t, err)
	_, err = l.db.Exec(`UPDATE evidence_units SET review_state='deferred' WHERE id=?`, units[2].ID)
	check(t, err)
	var pendingChars, pendingBytes int64
	for i, u := range units {
		if i != 0 && i != 2 {
			pendingChars += int64(utf8.RuneCountInString(u.Text))
			pendingBytes += int64(len(u.Text))
		}
	}
	status, err := l.Status()
	check(t, err)
	if status.PendingChars != pendingChars || status.Coverage.Pending.Bytes != pendingBytes || status.PendingSources != 1 {
		t.Fatalf("incorrect pending Unicode counts: got chars=%d bytes=%d; want chars=%d bytes=%d", status.PendingChars, status.Coverage.Pending.Bytes, pendingChars, pendingBytes)
	}
}

func BenchmarkStatusEscapedSource(b *testing.B) {
	l, err := Open(context.Background(), b.TempDir(), "escaped")
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	if _, err = l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("\x01", 500000)}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		status, err := l.Status()
		if err != nil {
			b.Fatal(err)
		}
		if status.PendingChars != 500000 {
			b.Fatal("incorrect character count")
		}
	}
}
