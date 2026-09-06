package ledger

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type reconstructed struct {
	fields    map[string]string
	relations map[string][]string
	sources   map[string]string
	headers   map[string]RecordHeader
	evidence  []EvidenceUnit
}

func drainRecall(t *testing.T, l *Ledger, id string) reconstructed {
	t.Helper()
	got := reconstructed{fields: map[string]string{}, relations: map[string][]string{}, sources: map[string]string{}, headers: map[string]RecordHeader{}}
	seen := map[string]bool{}
	token := ""
	for {
		p, err := l.ReadRecall(id, token)
		check(t, err)
		body, err := EncodeResponse(p)
		check(t, err)
		if len(body) > 12000 || !utf8.Valid(body) || !json.Valid(body) {
			t.Fatal("invalid envelope")
		}
		for _, i := range p.Page.Items {
			members := 0
			if h := i.Header; h != nil {
				members++
				if _, ok := got.headers[h.ID]; ok {
					t.Fatal("repeated header")
				}
				got.headers[h.ID] = *h
			}
			if f := i.Field; f != nil {
				members++
				key := f.RecordID + "/" + f.Field
				if int64(len(got.fields[key])) != f.StartByte || f.EndByte-f.StartByte != int64(len(f.Text)) || f.EndByte > f.TotalBytes {
					t.Fatal("nonadjacent field")
				}
				got.fields[key] += f.Text
			}
			if s := i.Support; s != nil {
				members++
				key := s.RecordID + "/" + s.Relation
				if s.Ordinal != int64(len(got.relations[key])) {
					t.Fatal("nonadjacent relation")
				}
				got.relations[key] = append(got.relations[key], s.TargetID)
			}
			if e := i.Evidence; e != nil {
				members++
				if !strings.HasPrefix(id, "e-") && int64(len(got.sources[e.SourceID])) != e.StartByte {
					t.Fatal("nonadjacent evidence")
				}
				got.sources[e.SourceID] += e.Text
				got.evidence = append(got.evidence, *e)
			}
			if members != 1 {
				t.Fatal("invalid inspection union")
			}
		}
		if p.Page.NextCursor == "" {
			break
		}
		if seen[p.Page.NextCursor] {
			t.Fatal("cursor did not advance")
		}
		seen[p.Page.NextCursor] = true
		token = p.Page.NextCursor
	}
	return got
}
func TestReadEnvelopeReconstructsClosure(t *testing.T) {
	l := openTest(t, t.TempDir(), "reconstruction")
	wantSources := map[string]string{}
	supports := []string{}
	observations := []string{}
	for n := range 20 {
		text := fmt.Sprintf("source %d\n", n) + strings.Repeat("界🙂\x00\"\\\n", 1000)
		c, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: fmt.Sprint(n)})
		check(t, err)
		wantSources[c.SourceID] = text
		units := readTestUnits(t, l, c.SourceID)
		for _, u := range units {
			if len(supports) < 100 {
				supports = append(supports, u.ID)
			}
		}
		r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: fmt.Sprintf("observation %d", n), EvidenceIDs: []string{c.FirstUnitID}}}}))
		check(t, err)
		observations = append(observations, r.Observations[0])
	}
	large := strings.Repeat("\x01界", 999)
	r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: large, EvidenceIDs: supports}}}))
	check(t, err)
	old := r.Observations[0]
	replacement, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "replacement", EvidenceIDs: supports}}}))
	check(t, err)
	reason := strings.Repeat("\x02界", 499)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: old, Reason: reason, ReplacementIDs: replacement.Observations}}}))
	check(t, err)
	got := drainRecall(t, l, old)
	if got.fields[old+"/text"] != large || got.fields[old+"/retirement.reason"] != reason || got.headers[old].Active {
		t.Fatal("retired fields lost")
	}
	canonical, _ := supportIDs(supports)
	if !reflect.DeepEqual(got.relations[old+"/evidence"], canonical) || !reflect.DeepEqual(got.relations[old+"/replacement"], replacement.Observations) {
		t.Fatal("relations changed")
	}
	reflection, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Reflections: []Reflection{{Text: "twenty sources", ObservationIDs: observations}}}))
	check(t, err)
	got = drainRecall(t, l, reflection.Reflections[0])
	if !reflect.DeepEqual(got.sources, wantSources) || len(got.headers) != 21 {
		t.Fatal("support closure incomplete")
	}
	for id, text := range wantSources {
		if drainRecall(t, l, id).sources[id] != text {
			t.Fatal("source reconstruction changed")
		}
		break
	}
	unit := drainRecall(t, l, supports[0])
	if len(unit.evidence) != 1 || unit.evidence[0].ID != supports[0] {
		t.Fatal("evidence recall expanded its scope")
	}
	for _, unknown := range []string{"", "o-missing", "s-missing", "e-missing", "r-missing"} {
		if _, err = l.ReadRecall(unknown, ""); err == nil {
			t.Fatal("unknown ID accepted", unknown)
		}
	}
}
func TestPaginationPendingSnapshots(t *testing.T) {
	l := openTest(t, t.TempDir(), "pages")
	text := strings.Repeat("界🙂\x00", 2000)
	c, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: "one"})
	check(t, err)
	first, err := l.ReadPending("")
	check(t, err)
	if first.Page.NextCursor == "" {
		t.Fatal("missing continuation")
	}
	token := first.Page.NextCursor
	_, err = l.CaptureV2(CaptureInput{Kind: "user", Text: "later", Key: "two"})
	check(t, err)
	got := ""
	p := first
	for {
		body, err := EncodeResponse(p)
		check(t, err)
		if len(body) > 12000 || len(p.Page.Items) > 24 || p.Through != 0 {
			t.Fatal("bad pending envelope")
		}
		for _, u := range p.Page.Items {
			if u.SourceID != c.SourceID {
				t.Fatal("append leaked into snapshot")
			}
			got += u.Text
		}
		if p.Page.NextCursor == "" {
			break
		}
		p, err = l.ReadPending(p.Page.NextCursor)
		check(t, err)
	}
	if got != text {
		t.Fatal("pending source lost")
	}
	other := openTest(t, l.store, "other")
	if _, err = other.ReadPending(token); err == nil {
		t.Fatal("cross-session cursor accepted")
	}
	if _, err = l.ReadRecall(c.SourceID, token); err == nil {
		t.Fatal("wrong command accepted")
	}
	before := checkpointSnapshot(t, l)
	if _, err = l.ReadPending(token + "x"); err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("malformed cursor mutated state")
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{SourceID: c.SourceID, Reason: "recover later"}}}))
	check(t, err)
	if _, err = l.ReadPending(token); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatal("stale cursor accepted")
	}
	p, err = l.ReadPending("")
	check(t, err)
	if p.Coverage.Deferred.Bytes != int64(len(text)) || p.Coverage.Pending.Units != 1 {
		t.Fatal("coverage hidden")
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: []string{p.Page.Items[0].ID}}))
	check(t, err)
	p, err = l.ReadPending("")
	check(t, err)
	if len(p.Page.Items) != 0 || p.Page.NextCursor != "" || p.Coverage.Pending.Units != 0 || p.Coverage.Reviewed.Units != 1 || p.Coverage.Deferred.Bytes != int64(len(text)) {
		t.Fatal("resolved queue hid coverage")
	}
	recalled := drainRecall(t, l, c.SourceID)
	for _, unit := range recalled.evidence {
		if unit.ReviewState != ReviewDeferred || unit.DeferralReason != "recover later" {
			t.Fatal("deferred audit disappeared")
		}
	}

}

func TestPaginationInspectionAndReopen(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "inspection")
	_, _, old := observe(t, l, strings.Repeat("\x01界", 999))
	_, _, newer := observe(t, l, "replacement")
	_, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: old, Reason: strings.Repeat("\x02", 999), ReplacementIDs: []string{newer}}}}))
	check(t, err)
	p, err := l.ReadEntries("")
	check(t, err)
	if p.Page.NextCursor == "" {
		t.Fatal("fixture must page")
	}
	oldToken := p.Page.NextCursor
	items := p.Page.Items
	check(t, l.Close())
	l = openTest(t, store, "inspection")
	for p.Page.NextCursor != "" {
		p, err = l.ReadEntries(p.Page.NextCursor)
		check(t, err)
		_, err = EncodeResponse(p)
		check(t, err)
		items = append(items, p.Page.Items...)
	}
	text, reason := "", ""
	headers := map[string]bool{}
	relations := []string{}
	seenEvidence := map[string]bool{}
	for _, i := range items {
		if i.Header != nil {
			if headers[i.Header.ID] {
				t.Fatal("duplicate header")
			}
			headers[i.Header.ID] = true
		}
		if i.Field != nil && i.Field.RecordID == old {
			switch i.Field.Field {
			case "text":
				if int64(len(text)) != i.Field.StartByte {
					t.Fatal("text gap")
				}
				text += i.Field.Text
			case "retirement.reason":
				if int64(len(reason)) != i.Field.StartByte {
					t.Fatal("reason gap")
				}
				reason += i.Field.Text
			}
		}
		if i.Support != nil && i.Support.RecordID == old && i.Support.Relation == "replacement" {
			if int64(len(relations)) != i.Support.Ordinal {
				t.Fatal("relation gap")
			}
			relations = append(relations, i.Support.TargetID)
		}
		if i.Evidence != nil {
			if seenEvidence[i.Evidence.ID] {
				t.Fatal("duplicate closure evidence")
			}
			seenEvidence[i.Evidence.ID] = true
		}
	}
	if text != strings.Repeat("\x01界", 999) || reason != strings.Repeat("\x02", 999) || len(headers) != 2 || !reflect.DeepEqual(relations, []string{newer}) {
		t.Fatal("inspection omitted stored records")
	}
	// Invalid arguments, version, fields and epoch must fail without writes.
	if _, err = l.ReadRecall(old, oldToken); err == nil {
		t.Fatal("wrong scope accepted")
	}
	c, err := decodeCursor(oldToken)
	check(t, err)
	c.Version++
	bad, err := encodeCursor(c)
	check(t, err)
	before := checkpointSnapshot(t, l)
	if _, err = l.ReadEntries(bad); err == nil || before != checkpointSnapshot(t, l) {
		t.Fatal("unsupported cursor mutated state")
	}
	check(t, l.SetState("pagination_epoch", "new-import-epoch"))
	if _, err = l.ReadEntries(oldToken); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatal("old epoch accepted")
	}
}

func TestReadEnvelopePrimePriorityAndState(t *testing.T) {
	l := openTest(t, t.TempDir(), strings.Repeat("界\"", 100))
	c, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Do not repeat completed work.", Key: "critical"})
	check(t, err)
	r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "Original critical constraint", Importance: "critical", EvidenceIDs: []string{c.FirstUnitID}}}}))
	check(t, err)
	critical := r.Observations[0]
	r, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "Priority-protected replacement", Importance: "low", EvidenceIDs: []string{c.FirstUnitID}}}}))
	check(t, err)
	replacement := r.Observations[0]
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: critical, Reason: "updated", ReplacementIDs: []string{replacement}}}}))
	check(t, err)
	for n := range 24 {
		_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: fmt.Sprintf("background %d %s", n, strings.Repeat("界", 1800)), EvidenceIDs: []string{c.FirstUnitID}}}}))
		check(t, err)
	}
	// This critical record is legal to store, but its quoted body alone exceeds
	// the rendering envelope. It must stay exactly recallable and visibly omitted.
	oversized := strings.Repeat("\u200b", 2000)
	r, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: oversized, Importance: "critical", EvidenceIDs: []string{c.FirstUnitID}}}, WorkingState: &WorkingState{Objective: &WorkingFact{Text: strings.Repeat("next step ", 180), EvidenceIDs: []string{c.FirstUnitID}}}}))
	check(t, err)
	state, err := l.Status()
	check(t, err)
	for _, render := range []func() (string, error){l.View, l.Prime} {
		view, err := render()
		check(t, err)
		body, err := EncodeResponse(view)
		check(t, err)
		if len(body) > 12000 || !strings.Contains(view, "Priority-protected replacement") || !strings.Contains(view, "1 critical entries omitted") || strings.Contains(view, "Original critical constraint") {
			t.Fatal("priority/overflow rendering wrong")
		}
	}
	prime, err := l.Prime()
	check(t, err)
	if !strings.Contains(prime, c.FirstUnitID) || !strings.Contains(prime, "Working state metadata:") || !strings.Contains(prime, fmt.Sprintf(`"revision":%d`, state.WorkingState.Revision)) || !strings.Contains(prime, `"deferred":{"units":0,"bytes":0}`) {
		t.Fatal("prime lost state or explicit coverage")
	}
	inspected := drainRecall(t, l, replacement)
	if inspected.headers[replacement].Importance != "low" || inspected.headers[replacement].EffectiveImportance != "critical" {
		t.Fatal("priority changed immutable record importance")
	}
	if drainRecall(t, l, r.Observations[0]).fields[r.Observations[0]+"/text"] != oversized {
		t.Fatal("oversized critical text lost")
	}
	originalStore := l.store
	l.store = strings.Repeat("\n", 7000)
	if _, err = l.Prime(); err == nil {
		t.Fatal("oversized header accepted")
	}
	l.store = originalStore
}
