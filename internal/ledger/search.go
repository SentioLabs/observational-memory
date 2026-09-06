package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Modes are ordered, not blended: BM25 from the memory and source corpora is
// not calibrated across indexes. Each corpus orders by score, kind, seq, ID.
// MatchOrdinal is zero based; UnitOrdinal is the last emitted evidence seq.
type rankedSearchPosition struct {
	Mode         int     `json:"mode"`
	BM25         float64 `json:"bm25"`
	Kind         string  `json:"kind"`
	Seq          int64   `json:"seq"`
	ID           string  `json:"id"`
	MatchOrdinal int     `json:"match_ordinal"`
	UnitOrdinal  int64   `json:"unit_ordinal"`
}

func literalQuery(query string) string {
	parts := []string{}
	for _, word := range strings.Fields(query) {
		// unicode61 indexes letters, numbers and private-use characters. Punctuation
		// alone contributes no searchable token and must not turn an AND into false.
		if !strings.ContainsFunc(word, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r) }) {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " AND ")
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

func (l *Ledger) ReadSearch(query string, options SearchOptions) (SearchPage, error) {
	result := SearchPage{}
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) {
		return result, fmt.Errorf("search query must be UTF-8 text without NUL")
	}
	q, close, err := l.readSnapshot()
	if err != nil {
		return result, err
	}
	defer close()
	arguments, err := JSON([]any{query, options.IncludeRetired, options.Sources})
	if err != nil {
		return result, err
	}
	c, err := l.pageCursor(q, options.Cursor, "search", string(arguments), true)
	if err != nil {
		return result, err
	}
	if c.Position.Record != 0 || c.Position.Offset != 0 || c.Position.Evidence != 0 || (c.Position.Phase != "" && c.Position.Phase != "search") {
		return result, fmt.Errorf("invalid search position; restart pagination")
	}
	if p := c.Position.Search; p != nil {
		if c.Position.Phase != "search" || p.Mode < 0 || p.Mode > 1 || (p.Mode == 1 && !options.Sources) || math.IsNaN(p.BM25) || math.IsInf(p.BM25, 0) || p.Seq <= 0 || p.ID == "" || p.MatchOrdinal < 0 || p.UnitOrdinal < 0 || (p.Mode == 0 && (p.MatchOrdinal != 0 || p.UnitOrdinal != 0)) {
			return result, fmt.Errorf("invalid search position; restart pagination")
		}
	}
	stream := searchStream{l: l, q: q, cursor: c, query: literalQuery(query), options: options}
	result.Page, err = packPage(c, ReadEnvelopeBytes, stream.next, func(p Page[SearchHit]) any { return SearchPage{Page: p} })
	return result, err
}

type searchSource struct {
	position rankedSearchPosition
	text     string
	spans    []byteRange
}
type searchStream struct {
	l       *Ledger
	q       queryer
	cursor  readCursor
	query   string
	options SearchOptions
	cached  *searchSource
}

func (s *searchStream) next(p readPosition) (SearchHit, readPosition, bool, error) {
	empty := SearchHit{}
	if s.query == "" {
		return empty, p, false, nil
	}
	position := rankedSearchPosition{}
	if p.Search != nil {
		position = *p.Search
	}
	if position.Mode == 0 {
		hit, after, err := s.memory(position)
		if err == nil {
			p.Phase = "search"
			p.Search = &after
			return hit, p, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return empty, p, false, err
		}
		position = rankedSearchPosition{Mode: 1}
	}
	if !s.options.Sources {
		return empty, p, false, nil
	}
	for {
		src, err := s.source(position)
		if errors.Is(err, sql.ErrNoRows) {
			return empty, p, false, nil
		}
		if err != nil {
			return empty, p, false, err
		}
		startMatch, afterUnit := 0, int64(0)
		if src.position.Seq == position.Seq {
			startMatch = position.MatchOrdinal
			afterUnit = position.UnitOrdinal
		}
		for i := startMatch; i < len(src.spans); i++ {
			span := src.spans[i]
			u, err := scanEvidence(s.q.QueryRowContext(s.l.ctx, evidenceSelect+` WHERE u.source_id=? AND u.seq>? AND u.end_byte>? AND u.start_byte<? ORDER BY u.seq LIMIT 1`, src.position.ID, afterUnit, span.start, span.end))
			if errors.Is(err, sql.ErrNoRows) {
				afterUnit = 0
				continue
			}
			if err != nil {
				return empty, p, false, err
			}
			start, end := max(int64(span.start), u.StartByte), min(int64(span.end), u.EndByte)
			hit := SearchHit{ID: src.position.ID, Kind: "source", Active: true, SourceID: src.position.ID, StartByte: start, EndByte: end, Snippet: src.text[start:end], Evidence: &u}
			after := src.position
			after.MatchOrdinal = i
			after.UnitOrdinal = u.Seq
			p.Phase = "search"
			p.Search = &after
			return hit, p, true, nil
		}
		// All intersections of this source have been emitted. Seek the next rank.
		position = src.position
		position.MatchOrdinal = len(src.spans)
		s.cached = nil
	}
}

func (s *searchStream) memory(p rankedSearchPosition) (SearchHit, rankedSearchPosition, error) {
	hit := SearchHit{}
	after := rankedSearchPosition{}
	// SQL is fixed; every query and cursor value is bound. LIMIT bounds the row
	// fetch, even when a large retained corpus matches the literal query.
	query := `SELECT e.seq,e.id,json_extract(e.body,'$.kind'),e.active,COALESCE(e.retirement,''),bm25(entry_fts),json_extract(e.body,'$.text')
 FROM entry_fts JOIN entries e ON e.seq=entry_fts.rowid
 WHERE entry_fts MATCH ? AND (? OR e.active=1)
 AND (?=0 OR (bm25(entry_fts),json_extract(e.body,'$.kind'),e.seq,e.id)>(?,?,?,?))
 ORDER BY bm25(entry_fts),json_extract(e.body,'$.kind'),e.seq,e.id LIMIT 1`
	var retirement, original string
	err := s.q.QueryRowContext(s.l.ctx, query, s.query, s.options.IncludeRetired, p.Seq, p.BM25, p.Kind, p.Seq, p.ID).Scan(&after.Seq, &hit.ID, &hit.Kind, &hit.Active, &retirement, &after.BM25, &original)
	if err != nil {
		return hit, after, err
	}
	after.ID, after.Kind = hit.ID, hit.Kind
	if retirement != "" {
		var r Retirement
		if err = Decode([]byte(retirement), &r); err != nil {
			return hit, after, err
		}
		hit.ReplacementIDs = r.ReplacementIDs
	}
	spans, err := s.highlight(0, after.Seq, original)
	if err != nil {
		return hit, after, err
	}
	if len(spans) > 0 {
		start := max(0, spans[0].start-80)
		for start > 0 && !utf8.RuneStart(original[start]) {
			start--
		}
		pieces, err := segmentText(original[start:])
		if err != nil {
			return hit, after, err
		}
		hit.Snippet = original[start : start+pieces[0].end]
	}

	return hit, after, nil
}

func (s *searchStream) source(p rankedSearchPosition) (*searchSource, error) {
	if s.cached != nil && s.cached.position.Seq == p.Seq {
		return s.cached, nil
	}
	src := &searchSource{position: rankedSearchPosition{Mode: 1, Kind: "source"}}
	// Resume the same source only while it may have more match/unit intersections.
	same := p.Seq > 0 && p.UnitOrdinal > 0
	query := `SELECT s.seq,s.id,bm25(source_fts),json_extract(s.body,'$.text')
 FROM source_fts JOIN sources s ON s.seq=source_fts.rowid
 WHERE source_fts MATCH ? AND s.seq<=?
 AND (?=0 OR (? AND s.seq=?) OR (bm25(source_fts),s.seq,s.id)>(?,?,?))
 ORDER BY bm25(source_fts),s.seq,s.id LIMIT 1`
	err := s.q.QueryRowContext(s.l.ctx, query, s.query, s.cursor.SourceHighWater, p.Seq, same, p.Seq, p.BM25, p.Seq, p.ID).Scan(&src.position.Seq, &src.position.ID, &src.position.BM25, &src.text)
	if err != nil {
		return nil, err
	}
	src.spans, err = s.highlight(1, src.position.Seq, src.text)
	if err != nil {
		return nil, err
	}
	s.cached = src
	return src, nil
}

func (s *searchStream) highlight(mode int, seq int64, original string) ([]byteRange, error) {
	var openMarker, closeMarker string
	for i := 0; ; i++ {
		openMarker = fmt.Sprintf("<om-match-%d>", i)
		closeMarker = fmt.Sprintf("</om-match-%d>", i)
		if !strings.Contains(original, openMarker) && !strings.Contains(original, closeMarker) {
			break
		}
	}
	query := `SELECT highlight(entry_fts,0,?,?) FROM entry_fts WHERE entry_fts MATCH ? AND rowid=?`
	if mode == 1 {
		query = `SELECT highlight(source_fts,0,?,?) FROM source_fts WHERE source_fts MATCH ? AND rowid=?`
	}
	var highlighted string
	if err := s.q.QueryRowContext(s.l.ctx, query, openMarker, closeMarker, s.query, seq).Scan(&highlighted); err != nil {
		return nil, err
	}
	return matchSpans(original, highlighted, openMarker, closeMarker)
}
