package ledger

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
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
		var indexed string
		check(t, l.db.QueryRow(`SELECT text FROM source_fts ORDER BY rowid DESC LIMIT 1`).Scan(&indexed))
		if indexed != text {
			t.Fatal("index did not retain complete original text")
		}
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

// A subprocess deadline catches a regression even if an SQLite auxiliary
// function does not promptly observe cancellation during a long C-API call.
func TestSearchDenseSourcePage(t *testing.T) {
	if os.Getenv("OM_TEST_DENSE_SEARCH") == "1" {
		l := openTest(t, t.TempDir(), "dense")
		_, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("needle ", 100000), Key: "dense"})
		check(t, err)
		started := time.Now()
		first, err := l.ReadSearch("needle", SearchOptions{Sources: true})
		check(t, err)
		if len(first.Page.Items) == 0 || first.Page.NextCursor == "" {
			t.Fatal("missing bounded dense page")
		}
		second, err := l.ReadSearch("needle", SearchOptions{Sources: true, Cursor: first.Page.NextCursor})
		check(t, err)
		if len(second.Page.Items) == 0 || second.Page.Items[0].StartByte <= first.Page.Items[len(first.Page.Items)-1].StartByte {
			t.Fatal("dense continuation lost position")
		}
		t.Logf("first and continued dense pages: %s", time.Since(started))
		tokenizer, err := newSearchTokenizer()
		check(t, err)
		defer tokenizer.close()
		matcher, err := newSearchPhrases(l.ctx, tokenizer, []string{"needle"})
		check(t, err)
		spans, err := matcher.spans(l.ctx, tokenizer, strings.Repeat("needle ", 100000))
		check(t, err)
		if len(spans) != 100000 || spans[0] != (byteRange{0, 6}) || spans[len(spans)-1] != (byteRange{699993, 699999}) {
			t.Fatal("dense spans were capped or dropped")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSearchDenseSourcePage$", "-test.v")
	cmd.Env = append(os.Environ(), "OM_TEST_DENSE_SEARCH=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dense source page failed (deadline: %v): %v\n%s", ctx.Err(), err, output)
	}
	t.Log(string(output))
}

// matchSpans validates the entire reconstruction before trusting any offset.
// Markers must be absent from the original, including literal marker-like logs.
func matchSpans(original, highlighted, openMarker, closeMarker string) ([]byteRange, error) {
	bad := fmt.Errorf("search highlight does not reconstruct retained text")
	if openMarker == "" || closeMarker == "" || openMarker == closeMarker || strings.Contains(original, openMarker) || strings.Contains(original, closeMarker) {
		return nil, bad
	}
	var reconstructed strings.Builder
	spans := []byteRange{}
	for {
		at := strings.Index(highlighted, openMarker)
		if at < 0 {
			if strings.Contains(highlighted, closeMarker) {
				return nil, bad
			}
			reconstructed.WriteString(highlighted)
			break
		}
		plain := highlighted[:at]
		if strings.Contains(plain, closeMarker) {
			return nil, bad
		}
		reconstructed.WriteString(plain)
		highlighted = highlighted[at+len(openMarker):]
		end := strings.Index(highlighted, closeMarker)
		if end <= 0 || strings.Contains(highlighted[:end], openMarker) {
			return nil, bad
		}
		start := reconstructed.Len()
		reconstructed.WriteString(highlighted[:end])
		spans = append(spans, byteRange{start, reconstructed.Len()})
		highlighted = highlighted[end+len(closeMarker):]
	}
	// Only the FTS representation changes NUL to U+0001, both unicode61
	// separators of equal UTF-8 width. Validate it, then restore precisely the
	// original NUL positions (preserving preexisting U+0001) and verify base bytes.
	restored := []byte(reconstructed.String())
	if string(restored) != strings.ReplaceAll(original, "\x00", "\x01") {
		return nil, bad
	}
	for i := 0; i < len(original); i++ {
		if original[i] == 0 {
			restored[i] = 0
		}
	}
	if string(restored) != original {
		return nil, bad
	}
	for _, span := range spans {
		if !utf8.ValidString(original[span.start:span.end]) {
			return nil, bad
		}
	}
	return spans, nil
}

func TestSearchTokenSpansMatchSQLite(t *testing.T) {
	l := openTest(t, t.TempDir(), "token-oracle")
	_, err := l.db.Exec(`CREATE VIRTUAL TABLE temp.search_oracle USING fts5(text, tokenize='unicode61')`)
	check(t, err)
	tokenizer, err := newSearchTokenizer()
	check(t, err)
	defer tokenizer.close()
	cases := []struct{ text, query string }{
		{strings.Repeat("a", 40000), strings.Repeat("a", 32768)},
		{strings.Repeat("a", 32768), strings.Repeat("a", 40000)},
		{strings.Repeat("é", 40000), strings.Repeat("é", 32768)},
		{strings.Repeat("界", 14000), strings.Repeat("界", 11000)},
		{strings.Repeat("界", 14000), strings.Repeat("界", 10922) + "畍"},
		{"a b c a b c", "a_b b_c"},
		{"a b c x b x c", "b a_b_c c"},
		{"a b a b", "a b"},
		{"Café CAFÉ cafe cãfé", "cafe"},
		{"Άλφα άλφα АЛЬФА альфа", "Άλφα АЛЬФА"},
		{"中文🙂 中文🙂", "中文🙂"},
		{"a\x00b a\x01b tail\x00", "a_b"},
		{"<om-match-0> E PIPE 742 </om-match-0> E_PIPE_742", "E_PIPE_742"},
		{strings.Repeat("界", 680) + " E PIPE 742 " + strings.Repeat("🙂", 900), "E_PIPE_742"},
		{"a " + strings.Repeat("b ", 700) + "c", "a_" + strings.Repeat("b_", 700) + "c"},
	}
	rng := rand.New(rand.NewSource(742))
	alphabet := []string{"a", "b", "c", "CAFÉ", "中文", "é", "OR"}
	for i := 0; i < 200; i++ {
		tokens := make([]string, 50)
		for j := range tokens {
			tokens[j] = alphabet[rng.Intn(len(alphabet))]
		}
		terms := make([]string, 4)
		for j := range terms {
			start := rng.Intn(46)
			terms[j] = strings.Join(tokens[start:start+1+rng.Intn(4)], "_")
		}
		cases = append(cases, struct{ text, query string }{strings.Join(tokens, " "), strings.Join(terms, " ")})
	}
	for i, tc := range cases {
		_, err = l.db.Exec(`DELETE FROM search_oracle`)
		check(t, err)
		// SQLite highlight drops unmatched NUL suffixes. Equal-width separator
		// normalization is only an oracle workaround, never production storage.
		_, err = l.db.Exec(`INSERT INTO search_oracle(text) VALUES(?)`, strings.ReplaceAll(tc.text, "\x00", "\x01"))
		check(t, err)
		var highlighted string
		check(t, l.db.QueryRow(`SELECT highlight(search_oracle,0,'<oracle-open>','<oracle-close>') FROM search_oracle WHERE search_oracle MATCH ?`, literalQuery(tc.query)).Scan(&highlighted))
		want, err := matchSpans(tc.text, highlighted, "<oracle-open>", "<oracle-close>")
		check(t, err)
		matcher, err := newSearchPhrases(l.ctx, tokenizer, literalWords(tc.query))
		check(t, err)
		got, err := matcher.spans(l.ctx, tokenizer, tc.text)
		check(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d query bytes=%d spans=%v want=%v", i, len(tc.query), got, want)
		}
	}
}

func TestSearchDenseCrossingPhraseCompleteness(t *testing.T) {
	l := openTest(t, t.TempDir(), "dense-phrase")
	// This long phrase repeatedly overlaps itself and crosses several units.
	// A separate tail match also ensures union does not merge adjacent matches.
	text := strings.Repeat("a ", 2600) + "tail"
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: "overlap"})
	check(t, err)
	hits := searchAll(t, l, "a_a tail", SearchOptions{Sources: true})
	var reconstructed string
	units := readTestUnits(t, l, log.SourceID)
	if len(hits) != len(units)+1 {
		t.Fatalf("got %d hits for %d units", len(hits), len(units))
	}
	for i, hit := range hits {
		if i < len(units) {
			reconstructed += hit.Snippet
		} else if hit.Snippet != "tail" {
			t.Fatal(hit)
		}
	}
	if reconstructed != strings.TrimSuffix(text, " tail") {
		t.Fatalf("overlap span lost bytes: %d", len(reconstructed))
	}
}

func TestSearchTokenizerLifetimeAndCancellation(t *testing.T) {
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			tokenizer, err := newSearchTokenizer()
			check(t, err)
			defer tokenizer.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			count := 0
			err = tokenizer.visit(ctx, strings.Repeat("é ", 50), 4, func(token string, start, end int) {
				if token != "e" || end-start != 2 {
					t.Errorf("bad token %q [%d,%d)", token, start, end)
				}
				count++
				if count == 2 {
					runtime.GC()
				}
				if count == 4 {
					cancel()
				}
			})
			if err != context.Canceled || count != 4 {
				t.Fatalf("cancellation: count=%d error=%v", count, err)
			}
			// The tokenizer can be used again after abort; no visitor or input survives.
			count = 0
			err = tokenizer.visit(context.Background(), "café", 4, func(token string, _, _ int) {
				if token != "cafe" {
					t.Error(token)
				}
				count++
			})
			check(t, err)
			if count != 1 {
				t.Fatal("tokenizer did not recover after cancellation")
			}
		})
	}
}

func TestSearchConcurrentIndependentReaders(t *testing.T) {
	store := t.TempDir()
	writer := openTest(t, store, "parallel-readers")
	_, err := writer.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("needle CAFÉ 中文🙂\x00 ", 30), Key: "shared"})
	check(t, err)
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			reader := openTest(t, store, "parallel-readers")
			hits := searchAll(t, reader, "cafe", SearchOptions{Sources: true})
			if len(hits) != 30 {
				t.Fatalf("parallel reader got %d hits", len(hits))
			}
			for _, hit := range hits {
				if hit.Snippet != "CAFÉ" {
					t.Fatal("parallel tokenizer mixed source spans")
				}
			}
		})
	}
}

func TestSearchQueryUsesSQLiteTokenCategories(t *testing.T) {
	l := openTest(t, t.TempDir(), "sqlite-query")
	_, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: "🙂 🫠 needle", Key: "tokens"})
	check(t, err)
	for _, query := range []string{"🙂", "🫠", "🙂 * --"} {
		hits := searchAll(t, l, query, SearchOptions{Sources: true})
		if len(hits) != 1 {
			t.Fatalf("query %q dropped a SQLite unicode61 token: %v", query, hits)
		}
	}
}

func TestSearchOversizedTokenKeys(t *testing.T) {
	cases := []struct {
		name, text, query string
		matches           bool
	}{
		{"ASCII document cap", strings.Repeat("a", 40000), strings.Repeat("a", 32768), true},
		{"ASCII query cap", strings.Repeat("a", 32768), strings.Repeat("a", 40000), true},
		{"normalize before cap", strings.Repeat("é", 40000), strings.Repeat("é", 32768), true},
		{"UTF8 byte split", strings.Repeat("界", 14000), strings.Repeat("界", 11000), true},
		{"UTF8 key suffix collision", strings.Repeat("界", 14000), strings.Repeat("界", 10922) + "畍", true},
		{"UTF8 shorter key stays distinct", strings.Repeat("界", 14000), strings.Repeat("界", 10922), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := openTest(t, t.TempDir(), "oversized")
			log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: tc.text, Key: "one-token"})
			check(t, err)
			var indexedMatches int
			check(t, l.db.QueryRow(`SELECT count(*) FROM source_fts WHERE source_fts MATCH ?`, literalQuery(tc.query)).Scan(&indexedMatches))
			if (indexedMatches == 1) != tc.matches {
				t.Fatalf("SQLite oracle matched %d rows", indexedMatches)
			}
			hits := searchAll(t, l, tc.query, SearchOptions{Sources: true})
			if !tc.matches {
				if len(hits) != 0 {
					t.Fatal("short key incorrectly matches capped token")
				}
				return
			}
			var reconstructed strings.Builder
			next := int64(0)
			for _, hit := range hits {
				if hit.SourceID != log.SourceID || hit.StartByte != next {
					t.Fatal("oversized token has missing or unordered evidence")
				}
				reconstructed.WriteString(hit.Snippet)
				next = hit.EndByte
			}
			if reconstructed.String() != tc.text || len(hits) != len(readTestUnits(t, l, log.SourceID)) {
				t.Fatalf("recovered %d of %d token bytes in %d hits", reconstructed.Len(), len(tc.text), len(hits))
			}
		})
	}
}
