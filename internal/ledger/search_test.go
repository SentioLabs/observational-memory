package ledger

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func searchAll(t *testing.T, l *Ledger, query string, opts SearchOptions) []SearchHit {
	t.Helper()
	var hits []SearchHit
	seen := map[string]bool{}
	for pageNo := 0; ; pageNo++ {
		if pageNo > 1000 {
			t.Fatal("search did not terminate")
		}
		page, err := l.ReadSearch(query, opts)
		check(t, err)
		encoded, err := EncodeResponse(page)
		check(t, err)
		if len(encoded) > ReadEnvelopeBytes || !utf8.Valid(encoded) {
			t.Fatal("unbounded search")
		}
		for _, hit := range page.Page.Items {
			key := fmt.Sprintf("%s/%d/%d", hit.ID, hit.StartByte, hit.EndByte)
			if seen[key] {
				t.Fatalf("duplicate hit %s", key)
			}
			seen[key] = true
			if hit.Kind == "source" {
				if hit.Evidence == nil || hit.SourceID != hit.Evidence.SourceID {
					t.Fatal("missing evidence")
				}
				src, err := source(l.ctx, l.db, hit.SourceID)
				check(t, err)
				if src.Text[hit.StartByte:hit.EndByte] != hit.Snippet || hit.StartByte < hit.Evidence.StartByte || hit.EndByte > hit.Evidence.EndByte || !utf8.ValidString(hit.Snippet) {
					t.Fatal("untraceable snippet")
				}
			}
			hits = append(hits, hit)
		}
		if page.Page.NextCursor == "" {
			return hits
		}
		if len(page.Page.Items) == 0 || page.Page.NextCursor == opts.Cursor {
			t.Fatal("no progress")
		}
		opts.Cursor = page.Page.NextCursor
	}
}

func TestSearchActiveRetiredAndDeferredEvidence(t *testing.T) {
	l := openTest(t, t.TempDir(), "search")
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("padding ", 4000) + "E_PIPE_742 中文🙂 oldalias" + strings.Repeat(" trailing", 4000), Key: "log"})
	check(t, err)
	r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{SourceID: log.SourceID, Reason: "bulk log; retrieve on demand"}}, Observations: []ObservationV2{{Text: "oldalias decision", EvidenceIDs: []string{log.FirstUnitID}}, {Text: "newalias decision", EvidenceIDs: []string{log.FirstUnitID}}}}))
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: r.Observations[0], Reason: "renamed", ReplacementIDs: []string{r.Observations[1]}}}}))
	check(t, err)
	if hits := searchAll(t, l, "oldalias", SearchOptions{}); len(hits) != 0 {
		t.Fatal(hits)
	}
	hits := searchAll(t, l, "oldalias", SearchOptions{IncludeRetired: true})
	if len(hits) != 1 || hits[0].Active || len(hits[0].ReplacementIDs) != 1 || hits[0].ReplacementIDs[0] != r.Observations[1] {
		t.Fatal(hits)
	}
	hits = searchAll(t, l, "E_PIPE_742", SearchOptions{Sources: true})
	if len(hits) != 1 || hits[0].Snippet != "E_PIPE_742" || hits[0].Evidence.ReviewState != ReviewDeferred || hits[0].Evidence.DeferralReason == "" {
		t.Fatal(hits)
	}
	if hits := searchAll(t, l, "中文🙂", SearchOptions{Sources: true}); len(hits) != 1 {
		t.Fatal(hits)
	}
}

func TestSearchLiteralAndCrossingSpans(t *testing.T) {
	l := openTest(t, t.TempDir(), "literal")
	text := strings.Repeat(" ", 2040) + "E_PIPE_742 <om-match-0> </om-match-0> 中文🙂 OR \"quoted\" (thing) $(touch /tmp/nope) SELECT * FROM entries; --"
	_, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: "one"})
	check(t, err)
	for _, query := range []string{"E_PIPE_742", "OR", `"quoted"`, "(thing)", "$(touch /tmp/nope)", "SELECT * FROM entries; --", "中文🙂"} {
		t.Run(query, func(t *testing.T) {
			if len(searchAll(t, l, query, SearchOptions{Sources: true})) == 0 {
				t.Fatal("literal query missed")
			}
		})
	}
	for _, query := range []string{"", "   ", "() * -- \""} {
		if hits := searchAll(t, l, query, SearchOptions{Sources: true}); len(hits) != 0 {
			t.Fatal(hits)
		}
	}
}

func TestSearchCursorPaginationAndInvalidation(t *testing.T) {
	for _, mutation := range []string{"append", "retire", "review", "query", "options", "session"} {
		t.Run(mutation, func(t *testing.T) {
			l := openTest(t, t.TempDir(), "paging")
			log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("needle "+strings.Repeat("界", 500)+" ", 35), Key: "log"})
			check(t, err)
			r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{SourceID: log.SourceID, Reason: "bulk"}}, Observations: []ObservationV2{{Text: "needle memory", EvidenceIDs: []string{log.FirstUnitID}}, {Text: "replacement", EvidenceIDs: []string{log.FirstUnitID}}}}))
			check(t, err)
			hits := searchAll(t, l, "needle", SearchOptions{Sources: true})
			if len(hits) != 36 || hits[0].Kind == "source" {
				t.Fatalf("mode ordering/count: %d", len(hits))
			}
			page, err := l.ReadSearch("needle", SearchOptions{Sources: true})
			check(t, err)
			if page.Page.NextCursor == "" {
				t.Fatal("missing cursor")
			}
			opts := SearchOptions{Sources: true, Cursor: page.Page.NextCursor}
			query := "needle"
			switch mutation {
			case "append":
				_, err = l.CaptureV2(CaptureInput{Kind: "user", Text: "unrelated corpus append", Key: "new"})
			case "retire":
				_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Retire: []Retirement{{ID: r.Observations[0], Reason: "fixed", ReplacementIDs: r.Observations[1:]}}}))
			case "review":
				_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{ReviewDeferred: []string{log.FirstUnitID}}))
			case "query":
				query = "replacement"
			case "options":
				opts.IncludeRetired = true
			case "session":
				l = openTest(t, l.store, "other")
			}
			check(t, err)
			if _, err = l.ReadSearch(query, opts); err == nil || !strings.Contains(err.Error(), "restart") {
				t.Fatalf("stale cursor accepted: %v", err)
			}
		})
	}
}

func TestSearchNULPreservesSourceOffsets(t *testing.T) {
	l := openTest(t, t.TempDir(), "nul")
	for i, text := range []string{"pre\x00omitted until E_PIPE_742 suffix", "E_PIPE_742 a\x00suffix remains", "a\x00before E_PIPE_742 after\x00tail", "E_PIPE_742\x00E_PIPE_742", "x\x01y\x00 E_PIPE_742 \x00end"} {
		_, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: fmt.Sprint(i)})
		check(t, err)
	}
	hits := searchAll(t, l, "E_PIPE_742", SearchOptions{Sources: true})
	if len(hits) != 6 {
		t.Fatalf("NUL source hits=%d", len(hits))
	}
	for _, hit := range hits {
		if hit.Snippet != "E_PIPE_742" {
			t.Fatal(hit)
		}
	}
}

func TestSearchCrossingAndMarkerCollisions(t *testing.T) {
	l := openTest(t, t.TempDir(), "crossing")
	prefix := "🙂中文 <om-match-0> </om-match-0> <om-match-1> </om-match-1> "
	prefix += strings.Repeat(" ", 2042-len(prefix))
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: prefix + "E_PIPE_742 tail", Key: "cross"})
	check(t, err)
	hits := searchAll(t, l, "E_PIPE_742", SearchOptions{Sources: true})
	if len(hits) != 2 || hits[0].Evidence.ID == hits[1].Evidence.ID || hits[0].Snippet+hits[1].Snippet != "E_PIPE_742" || hits[0].SourceID != log.SourceID || hits[0].EndByte != hits[1].StartByte {
		t.Fatal(hits)
	}
}

func TestSearchIndexAtomicity(t *testing.T) {
	l := openTest(t, t.TempDir(), "rollback")
	generation := func() string { v, err := l.State("search_generation"); check(t, err); return v }
	initial := generation()
	_, err := l.db.Exec(`CREATE TRIGGER fail_unit BEFORE INSERT ON evidence_units BEGIN SELECT RAISE(ABORT,'injected unit failure'); END`)
	check(t, err)
	if _, err = l.CaptureV2(CaptureInput{Kind: "tool", Text: "phantom rollback", Key: "bad"}); err == nil {
		t.Fatal("capture should fail")
	}
	if generation() != initial || len(searchAll(t, l, "phantom", SearchOptions{Sources: true})) != 0 {
		t.Fatal("failed capture changed search")
	}
	_, err = l.db.Exec(`DROP TRIGGER fail_unit`)
	check(t, err)
	input := CaptureInput{Kind: "tool", Text: "retained needle", Key: "good"}
	log, err := l.CaptureV2(input)
	check(t, err)
	saved := generation()
	_, err = l.CaptureV2(input)
	check(t, err)
	if generation() != saved {
		t.Fatal("duplicate capture changed corpus")
	}
	_, err = l.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON checkpoint_receipts BEGIN SELECT RAISE(ABORT,'injected commit failure'); END`)
	check(t, err)
	if _, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "phantom observation", EvidenceIDs: []string{log.FirstUnitID}}}})); err == nil {
		t.Fatal("apply should fail")
	}
	if generation() != saved || len(searchAll(t, l, "phantom", SearchOptions{})) != 0 {
		t.Fatal("failed apply changed search")
	}
	_, err = l.db.Exec(`DROP TRIGGER fail_receipt`)
	check(t, err)
	r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: []ObservationV2{{Text: "durable memory", EvidenceIDs: []string{log.FirstUnitID}}}}))
	check(t, err)
	if generation() == saved {
		t.Fatal("entry insert did not advance generation")
	}
	for _, table := range []string{"sources", "entries"} {
		saved = generation()
		tx, err := l.db.BeginTx(l.ctx, nil)
		check(t, err)
		if table == "sources" {
			_, err = tx.Exec(`DELETE FROM evidence_units`)
			check(t, err)
		}
		_, err = tx.Exec("DELETE FROM " + table)
		check(t, err)
		var count int
		fts := "source_fts"
		if table == "entries" {
			fts = "entry_fts"
		}
		check(t, tx.QueryRow("SELECT count(*) FROM "+fts).Scan(&count))
		if count != 0 {
			t.Fatal("deleted base still indexed")
		}
		check(t, tx.Rollback())
		if generation() != saved {
			t.Fatal("delete rollback changed generation")
		}
	}
	if hits := searchAll(t, l, "durable", SearchOptions{}); len(hits) != 1 || hits[0].ID != r.Observations[0] {
		t.Fatal(hits)
	}
	saved = generation()
	_, err = l.db.Exec(`UPDATE entries SET effective_priority=3 WHERE id=?`, r.Observations[0])
	check(t, err)
	if generation() == saved {
		t.Fatal("priority update did not invalidate ranking metadata")
	}
}

func TestSearchRankedMemoryAndSourcePages(t *testing.T) {
	l := openTest(t, t.TempDir(), "ranked")
	var observations []ObservationV2
	var sourceIDs []string
	for i := 0; i < 30; i++ {
		log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: "needle source", Key: fmt.Sprint(i)})
		check(t, err)
		sourceIDs = append(sourceIDs, log.SourceID)
		observations = append(observations, ObservationV2{Text: "needle " + strings.Repeat("界🙂\x02", 200), EvidenceIDs: []string{log.FirstUnitID}})
	}
	r, err := l.ApplyV2(checkpointNow(t, l, CheckpointV2{Observations: observations}))
	check(t, err)
	hits := searchAll(t, l, "needle", SearchOptions{Sources: true})
	if len(hits) != 60 {
		t.Fatalf("got %d hits", len(hits))
	}
	for i, id := range r.Observations {
		if hits[i].ID != id || hits[i].Kind != "observation" || !strings.Contains(hits[i].Snippet, "needle") {
			t.Fatalf("memory tie ordering at %d", i)
		}
	}
	for i, id := range sourceIDs {
		if hits[30+i].ID != id || hits[30+i].Kind != "source" {
			t.Fatalf("source tie ordering at %d", i)
		}
	}
	// A more frequent match ranks ahead of otherwise comparable source documents.
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: "needle needle", Key: "frequent"})
	check(t, err)
	hits = searchAll(t, l, "needle", SearchOptions{Sources: true})
	if hits[30].ID != log.SourceID {
		t.Fatal("BM25 frequency was not ranked ahead")
	}
}

func TestSearchLiteralOperatorsAndInvalidInput(t *testing.T) {
	l := openTest(t, t.TempDir(), "operators")
	_, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "alpha only", Key: "alpha"})
	check(t, err)
	if hits := searchAll(t, l, "alpha OR beta", SearchOptions{Sources: true}); len(hits) != 0 {
		t.Fatal("OR interpreted as operator")
	}
	for _, query := range []string{"needle\x00ignored", string([]byte{0xff})} {
		if _, err := l.ReadSearch(query, SearchOptions{}); err == nil {
			t.Fatal("invalid query accepted")
		}
	}
}
