package ledger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func checkpointNow(t *testing.T, l *Ledger, cp CheckpointV2) CheckpointV2 {
	t.Helper()
	status, err := l.Status()
	check(t, err)
	cp.ExpectedThrough, cp.ExpectedRevision = status.Through, status.Revision
	if cp.Acknowledge == nil {
		cp.Acknowledge = []string{}
	}
	return cp
}
func pendingIDs(t *testing.T, l *Ledger) []string {
	t.Helper()
	rows, err := l.db.Query(`SELECT id FROM evidence_units WHERE review_state='pending' ORDER BY seq`)
	check(t, err)
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		check(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	check(t, rows.Err())
	return ids
}
func evidenceID(t *testing.T, l *Ledger, source string) string {
	t.Helper()
	return readTestUnits(t, l, source)[0].ID
}
func checkpointSnapshot(t *testing.T, l *Ledger) string {
	t.Helper()
	var result []any
	for _, query := range []string{`SELECT key,value FROM meta ORDER BY key`, `SELECT seq,id,review_state,deferral_reason FROM evidence_units ORDER BY seq`, `SELECT seq,id,body,active,retirement,effective_priority FROM entries ORDER BY seq`, `SELECT * FROM working_state`, `SELECT digest,body FROM checkpoint_receipts ORDER BY digest`} {
		rows, err := l.db.Query(query)
		check(t, err)
		cols, err := rows.Columns()
		check(t, err)
		for rows.Next() {
			vals := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			check(t, rows.Scan(dest...))
			result = append(result, vals)
		}
		check(t, rows.Err())
		rows.Close()
	}
	body, err := json.Marshal(result)
	check(t, err)
	return string(body)
}
func TestCheckpointCoverage(t *testing.T) {
	cases := []struct {
		name string
		bad  bool
	}{
		{"valid oldest prefix", false}, {"gap skips unreviewed user", true}, {"duplicate acknowledgment", true},
		{"defer tool then acknowledge remaining prefix", false}, {"defer user", true}, {"defer assistant", true}, {"unknown source", true},
		{"acknowledge and defer same unit", true}, {"review_deferred overlaps acknowledgment", true},
		{"empty observations review routine evidence", false}, {"out of order citation without acknowledgment", false},
		{"64 combined operations", false}, {"65 combined operations", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := openTest(t, t.TempDir(), "coverage")
			u, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Original user constraint", Key: "u"})
			check(t, err)
			log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("界 log\n", 2000), Key: "l"})
			check(t, err)
			c, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "User correction", Key: "c"})
			check(t, err)
			a, err := l.CaptureV2(CaptureInput{Kind: "assistant", Text: "Acknowledged", Key: "a"})
			check(t, err)
			cp := checkpointNow(t, l, CheckpointV2{})
			switch tc.name {
			case "valid oldest prefix", "empty observations review routine evidence":
				cp.Acknowledge = []string{u.FirstUnitID}
			case "gap skips unreviewed user":
				cp.Acknowledge = []string{c.FirstUnitID}
			case "duplicate acknowledgment":
				cp.Acknowledge = []string{u.FirstUnitID, u.FirstUnitID}
			case "defer tool then acknowledge remaining prefix":
				cp.DeferSources = []SourceDeferral{{log.SourceID, "routine log"}}
				cp.Acknowledge = []string{u.FirstUnitID, c.FirstUnitID}
			case "defer user":
				cp.DeferSources = []SourceDeferral{{u.SourceID, "reason"}}
			case "defer assistant":
				cp.DeferSources = []SourceDeferral{{a.SourceID, "reason"}}
			case "unknown source":
				cp.DeferSources = []SourceDeferral{{"s-missing", "reason"}}
			case "acknowledge and defer same unit":
				cp.DeferSources = []SourceDeferral{{log.SourceID, "reason"}}
				cp.Acknowledge = []string{u.FirstUnitID, log.FirstUnitID}
			case "review_deferred overlaps acknowledgment":
				cp.Acknowledge = []string{u.FirstUnitID}
				cp.ReviewDeferred = []string{u.FirstUnitID}
			case "out of order citation without acknowledgment":
				cp.Observations = []ObservationV2{{Text: "Correction", EvidenceIDs: []string{c.FirstUnitID}}}
			default:
				n := 64
				if tc.bad {
					n = 65
				}
				for i := range n {
					cp.Observations = append(cp.Observations, ObservationV2{Text: fmt.Sprintf("Fact %d", i), EvidenceIDs: []string{c.FirstUnitID}})
				}
			}
			before := checkpointSnapshot(t, l)
			receipt, err := l.ApplyV2(cp)
			if tc.bad {
				if err == nil {
					t.Fatal("accepted invalid checkpoint")
				}
				if before != checkpointSnapshot(t, l) {
					t.Fatal("failed checkpoint mutated ledger")
				}
				return
			}
			check(t, err)
			if receipt.Revision != 1 {
				t.Fatal("revision not advanced")
			}
			if tc.name == "out of order citation without acknowledgment" && (receipt.Through != 0 || receipt.Coverage.Reviewed.Units != 0) {
				t.Fatal("citation changed coverage")
			}
			if tc.name == "defer tool then acknowledge remaining prefix" && (receipt.Coverage.Deferred.Units != log.UnitCount || receipt.Coverage.Reviewed.Units != 2) {
				t.Fatalf("bad coverage: %+v", receipt)
			}
		})
	}
}
func TestDeferralCoverage(t *testing.T) {
	l := openTest(t, t.TempDir(), "deferral")
	u, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Keep me pending", Key: "u"})
	check(t, err)
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("log\n", 4000), Key: "l"})
	check(t, err)
	cp := checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{log.SourceID, "Routine evidence retained"}}})
	r, err := l.ApplyV2(cp)
	check(t, err)
	if r.Through != 0 || r.Coverage.Deferred.Units != log.UnitCount {
		t.Fatal("deferral jumped earlier pending evidence")
	}
	r, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: []string{u.FirstUnitID}}))
	check(t, err)
	through := r.Through
	recalled, err := l.Recall(log.FirstUnitID)
	check(t, err)
	if recalled.Sources[0].Text != strings.Repeat("log\n", 4000) {
		t.Fatal("deferred source unavailable")
	}
	r, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{ReviewDeferred: []string{log.FirstUnitID}}))
	check(t, err)
	if r.Through != through || r.Coverage.Deferred.Units != log.UnitCount-1 || r.Coverage.Reviewed.Units != 2 {
		t.Fatal("deferred review changed cursor or coverage incorrectly")
	}
	if readTestUnits(t, l, log.SourceID)[0].DeferralReason != "Routine evidence retained" {
		t.Fatal("lost audit reason")
	}
}
func TestCheckpointRetry(t *testing.T) {
	l := openTest(t, t.TempDir(), "retry")
	s, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Constraint", Key: "u"})
	check(t, err)
	cp := checkpointNow(t, l, CheckpointV2{Acknowledge: []string{s.FirstUnitID}, Observations: []ObservationV2{{Text: "Original", EvidenceIDs: []string{s.FirstUnitID}}}})
	first, err := l.ApplyV2(cp)
	check(t, err)
	newer, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "Corrected", EvidenceIDs: []string{s.FirstUnitID}}}}))
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: first.Observations[0], Reason: "Correction", ReplacementIDs: newer.Observations}}}))
	check(t, err)
	before := checkpointSnapshot(t, l)
	retry, err := l.ApplyV2(cp)
	check(t, err)
	b1, err := EncodeResponse(first)
	check(t, err)
	b2, err := EncodeResponse(retry)
	check(t, err)
	if string(b1) != string(b2) || before != checkpointSnapshot(t, l) {
		t.Fatal("retry changed receipt or resurrected entry")
	}
	cp.Observations[0].Text = "Different stale request"
	if _, err = l.ApplyV2(cp); err == nil {
		t.Fatal("accepted stale request")
	}
}
func TestConcurrentCheckpoint(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "race")
	other := openTest(t, store, "race")
	s, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Evidence", Key: "u"})
	check(t, err)
	cp := checkpointNow(t, l, CheckpointV2{Acknowledge: []string{s.FirstUnitID}})

	type outcome struct {
		checkpoint CheckpointV2
		receipt    ReceiptV2
		err        error
	}
	var wg sync.WaitGroup
	results := make(chan outcome, 2)
	for i, w := range []*Ledger{l, other} {
		wg.Go(func() {
			c := cp
			c.Observations = []ObservationV2{{Text: fmt.Sprint(i), EvidenceIDs: []string{s.FirstUnitID}}}
			r, err := w.ApplyV2(c)
			results <- outcome{c, r, err}
		})
	}
	wg.Wait()
	close(results)
	n := 0
	var winner outcome
	for result := range results {
		if result.err == nil {
			n++
			winner = result
		}
	}
	if n != 1 {
		t.Fatalf("committed %d writers", n)
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &WorkingState{}}))
	check(t, err)
	before := checkpointSnapshot(t, l)
	retried, err := other.ApplyV2(winner.checkpoint)
	check(t, err)
	first, err := EncodeResponse(winner.receipt)
	check(t, err)
	second, err := EncodeResponse(retried)
	check(t, err)
	if string(first) != string(second) || before != checkpointSnapshot(t, l) {
		t.Fatal("winner retry changed receipt or later state")
	}
}

func TestWorkingStateAtomic(t *testing.T) {
	l := openTest(t, t.TempDir(), "state")
	s, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Approved objective", Key: "u"})
	check(t, err)
	state := &WorkingState{Objective: &WorkingFact{Text: "x", EvidenceIDs: []string{s.FirstUnitID}}}
	body, err := JSON(state)
	check(t, err)
	state.Objective.Text = strings.Repeat("x", WorkingStateBytes-len(body)+1)
	cp := checkpointNow(t, l, CheckpointV2{WorkingState: state})
	_, err = l.ApplyV2(cp)
	check(t, err)
	var saved string
	check(t, l.db.QueryRow(`SELECT body FROM working_state`).Scan(&saved))
	if len(saved) != 3500 {
		t.Fatalf("stored %d bytes", len(saved))
	}
	before := checkpointSnapshot(t, l)
	bad := *state
	fact := *state.Objective
	fact.Text += "x"
	bad.Objective = &fact
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &bad, Observations: []ObservationV2{{Text: "Rollback this", EvidenceIDs: []string{s.FirstUnitID}}}}))
	if err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("oversize state did not roll back")
	}
	fact.Text = "valid"
	fact.EvidenceIDs = []string{"e-missing"}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &bad}))
	if err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("invalid evidence did not roll back")
	}
	stale := cp
	stale.WorkingState = &WorkingState{}
	if _, err = l.ApplyV2(stale); err == nil {
		t.Fatal("stale state accepted")
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{}))
	check(t, err)
	var unchanged string
	check(t, l.db.QueryRow(`SELECT body FROM working_state`).Scan(&unchanged))
	if saved != unchanged {
		t.Fatal("nil state changed snapshot")
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &WorkingState{}}))
	check(t, err)
	check(t, l.db.QueryRow(`SELECT body FROM working_state`).Scan(&unchanged))
	var cleared WorkingState
	check(t, json.Unmarshal([]byte(unchanged), &cleared))
	if !reflect.DeepEqual(cleared, WorkingState{}) {
		t.Fatal("empty state did not clear")
	}
}

func TestReplacementPriority(t *testing.T) {
	l := openTest(t, t.TempDir(), "priority-floors")
	source, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "The current correction supersedes the critical constraint.", Key: "correction"})
	check(t, err)
	evidence := []string{source.FirstUnitID}
	created, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{
		{Text: "Original critical constraint", Importance: "critical", EvidenceIDs: evidence},
		{Text: "Corrected constraint", Importance: "medium", EvidenceIDs: evidence},
		{Text: "Latest corrected constraint", Importance: "low", EvidenceIDs: evidence},
		{Text: "Unrelated routine fact", Importance: "medium", EvidenceIDs: evidence},
	}}))
	check(t, err)
	old, middle, latest, routine := created.Observations[0], created.Observations[1], created.Observations[2], created.Observations[3]
	bodies := map[string]string{}
	for _, id := range created.Observations {
		var body string
		check(t, l.db.QueryRow(`SELECT body FROM entries WHERE id=?`, id).Scan(&body))
		bodies[id] = body
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{
		{ID: middle, Reason: "Newer correction", ReplacementIDs: []string{latest}},
		{ID: old, Reason: "Critical constraint corrected", ReplacementIDs: []string{middle}},
	}}))
	check(t, err)
	for _, id := range []string{old, middle, latest} {
		var floor int
		var body string
		check(t, l.db.QueryRow(`SELECT body,effective_priority FROM entries WHERE id=?`, id).Scan(&body, &floor))
		if floor != 3 || body != bodies[id] {
			t.Fatal("transitive replacement lost priority or changed identity/body")
		}
	}
	reflections, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Reflections: []Reflection{
		{Text: "Current critical correction", ObservationIDs: []string{latest}},
		{Text: "Routine conclusion", ObservationIDs: []string{routine}},
		{Text: "Newest summary of current correction", ObservationIDs: []string{latest}},
	}}))
	check(t, err)
	preserving, unrelated, last := reflections.Reflections[0], reflections.Reflections[1], reflections.Reflections[2]
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{
		{ID: preserving, Reason: "New summary", ReplacementIDs: []string{last}},
		{ID: latest, Reason: "Preserved by existing reflection", ReplacementIDs: []string{preserving}},
	}}))
	check(t, err)
	for _, tc := range []struct {
		id       string
		priority int
	}{{preserving, 3}, {unrelated, 1}, {last, 3}} {
		var priority int
		var importance string
		check(t, l.db.QueryRow(`SELECT effective_priority,json_extract(body,'$.importance') FROM entries WHERE id=?`, tc.id).Scan(&priority, &importance))
		if priority != tc.priority || importance != "medium" {
			t.Fatalf("reflection priority/original importance changed: %s %d %s", tc.id, priority, importance)
		}
	}
}

func TestCheckpointCoverageValidationRollback(t *testing.T) {
	l := openTest(t, t.TempDir(), "invalid-coverage")
	source, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("log\n", 2000), Key: "log"})
	check(t, err)
	units := readTestUnits(t, l, source.SourceID)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: []string{units[0].ID}, WorkingState: &WorkingState{Objective: &WorkingFact{Text: "Existing accepted objective", EvidenceIDs: []string{units[0].ID}}}}))
	check(t, err)
	cases := []CheckpointV2{
		{Acknowledge: []string{units[2].ID, units[1].ID}},
		{Acknowledge: []string{"e-missing"}},
		{DeferSources: []SourceDeferral{{source.SourceID, " "}}},
		{DeferSources: []SourceDeferral{{source.SourceID, strings.Repeat("界", 86)}}},
		{DeferSources: []SourceDeferral{{source.SourceID, "reason"}, {source.SourceID, "reason"}}},
		{DeferSources: []SourceDeferral{{source.SourceID, "reason"}}, ReviewDeferred: []string{units[1].ID}},
		{ReviewDeferred: []string{units[0].ID}},
		{ReviewDeferred: []string{"e-missing"}},
		{Acknowledge: []string{units[1].ID}, Observations: []ObservationV2{{Text: "Valid first mutation", EvidenceIDs: []string{units[0].ID}}}, Retire: []Retirement{{ID: "o-missing", Reason: "bad", ReplacementIDs: []string{"o-other"}}}, WorkingState: &WorkingState{}},
	}
	for i, cp := range cases {
		before := checkpointSnapshot(t, l)
		_, err = l.ApplyV2(checkpointNow(t, l, cp))
		if err == nil || before != checkpointSnapshot(t, l) {
			t.Fatalf("invalid operation %d changed ledger: %v", i, err)
		}
	}
	reason := strings.Repeat("界", 85) + "x"
	receipt, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{source.SourceID, reason}}}))
	check(t, err)
	if receipt.Coverage.Reviewed.Units != 1 || receipt.Coverage.Deferred.Units != source.UnitCount-1 {
		t.Fatal("deferral changed previously reviewed evidence")
	}
	for _, cp := range []CheckpointV2{{DeferSources: []SourceDeferral{{source.SourceID, "again"}}}, {ReviewDeferred: []string{units[1].ID, units[1].ID}}} {
		before := checkpointSnapshot(t, l)
		_, err = l.ApplyV2(checkpointNow(t, l, cp))
		if err == nil || before != checkpointSnapshot(t, l) {
			t.Fatal("invalid repeated operation changed ledger")
		}
	}
}

func TestCheckpointCoverageCombinedLimitAndLargeDeferral(t *testing.T) {
	for _, n := range []int{64, 65} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			l := openTest(t, t.TempDir(), "combined")
			log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("\x00", 400000), Key: "large"})
			check(t, err)
			if log.UnitCount < 1000 {
				t.Fatal("fixture must exercise thousands of units")
			}
			user, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Correction after routine log", Key: "user"})
			check(t, err)
			cp := checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{log.SourceID, "Bulk retained output"}}, Acknowledge: []string{user.FirstUnitID}, WorkingState: &WorkingState{Objective: &WorkingFact{Text: "Correction", EvidenceIDs: []string{user.FirstUnitID}}}})
			for i := range n - 3 {
				cp.Observations = append(cp.Observations, ObservationV2{Text: fmt.Sprintf("Fact %d", i), EvidenceIDs: []string{user.FirstUnitID, user.FirstUnitID}})
			}
			before := checkpointSnapshot(t, l)
			r, err := l.ApplyV2(cp)
			if n == 65 {
				if err == nil || before != checkpointSnapshot(t, l) {
					t.Fatal("over-limit mixed request mutated ledger")
				}
				return
			}
			check(t, err)
			body, err := EncodeResponse(r)
			check(t, err)
			if len(body) > ReadEnvelopeBytes || r.Coverage.Deferred.Units != log.UnitCount || r.Coverage.Deferred.Bytes != 400000 {
				t.Fatal("bulk deferral receipt lost aggregate coverage")
			}
			var saved []byte
			check(t, l.db.QueryRow(`SELECT body FROM checkpoint_receipts`).Scan(&saved))
			if string(body) != string(saved) || saved[len(saved)-1] != '\n' {
				t.Fatal("receipt persistence differs from complete response")
			}
		})
	}
}

func TestWorkingStateFactsRemainIndependent(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "state-lifecycle")
	source, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Approved constraint and completed action", Key: "u"})
	check(t, err)
	fact := WorkingFact{Text: "Verified completion", EvidenceIDs: []string{source.FirstUnitID}}
	cp := checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "Independent accepted constraint", EvidenceIDs: fact.EvidenceIDs}}, WorkingState: &WorkingState{Completed: []WorkingFact{fact}, Next: []WorkingFact{{Text: "Proposed next action, awaiting current instructions", EvidenceIDs: fact.EvidenceIDs}}}})
	_, err = l.ApplyV2(cp)
	check(t, err)
	if cp.WorkingState.Completed[0].Text != fact.Text {
		t.Fatal("checkpoint mutated caller state")
	}
	status, err := l.Status()
	check(t, err)
	if status.WorkingState == nil || status.WorkingState.Revision != 1 || status.Coverage.Pending.Units != source.UnitCount {
		t.Fatal("state metadata or citation coverage incorrect")
	}
	check(t, l.Close())
	l = openTest(t, store, "state-lifecycle")
	status, err = l.Status()
	check(t, err)
	unchanged, err := DecodeCheckpointV2([]byte(fmt.Sprintf(`{"expected_through":%d,"expected_revision":%d,"acknowledge":[],"working_state":null}`, status.Through, status.Revision)))
	check(t, err)
	_, err = l.ApplyV2(unchanged)
	check(t, err)
	var body string
	check(t, l.db.QueryRow(`SELECT body FROM working_state`).Scan(&body))
	if !strings.Contains(body, "Verified completion") {
		t.Fatal("null erased durable state")
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &WorkingState{Completed: []WorkingFact{fact}}}))
	check(t, err)
	entries, err := l.Entries(false)
	check(t, err)
	if len(entries) != 1 {
		t.Fatal("omission retired independent observation")
	}
	for _, bad := range []WorkingFact{{Text: " ", EvidenceIDs: fact.EvidenceIDs}, {Text: "Unsupported", EvidenceIDs: nil}} {
		before := checkpointSnapshot(t, l)
		_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{WorkingState: &WorkingState{Constraints: []WorkingFact{bad}}}))
		if err == nil || before != checkpointSnapshot(t, l) {
			t.Fatal("invalid working fact mutated state")
		}
	}
}

func TestDeferralCoverageRedactionBound(t *testing.T) {
	l := openTest(t, t.TempDir(), "deferral-redaction")
	source, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: "Routine log", Key: "l"})
	check(t, err)
	reason := strings.Repeat("x", 247) + " Bearer a"
	if len(reason) != MaxDeferralReasonBytes {
		t.Fatal("invalid boundary fixture")
	}
	before := checkpointSnapshot(t, l)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{source.SourceID, reason}}}))
	if err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("redaction expanded stored deferral past its byte bound")
	}
}

func TestReplacementPrioritySameCheckpointReflection(t *testing.T) {
	l := openTest(t, t.TempDir(), "composed-priority")
	source, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Correction", Key: "u"})
	check(t, err)
	evidence := []string{source.FirstUnitID}
	created, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{
		{Text: "Original critical", Importance: "critical", EvidenceIDs: evidence},
		{Text: "Medium replacement", Importance: "medium", EvidenceIDs: evidence},
	}}))
	check(t, err)
	reflected, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{
		Reflections: []Reflection{{Text: "Preserved correction", ObservationIDs: []string{created.Observations[1]}}},
		Retire:      []Retirement{{ID: created.Observations[0], Reason: "New correction", ReplacementIDs: []string{created.Observations[1]}}},
	}))
	check(t, err)
	var priority int
	check(t, l.db.QueryRow(`SELECT effective_priority FROM entries WHERE id=?`, reflected.Reflections[0]).Scan(&priority))
	if priority != 3 {
		t.Fatal("new reflection lost same-transaction support priority")
	}
}
