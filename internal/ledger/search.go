package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
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
	for _, word := range literalWords(query) {
		parts = append(parts, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " AND ")
}

func literalWords(query string) []string { return strings.Fields(query) }

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
	if stream.query != "" {
		stream.tokenizer, err = newSearchTokenizer()
		if err != nil {
			return result, err
		}
		defer stream.tokenizer.close()
		stream.matcher, err = newSearchPhrases(l.ctx, stream.tokenizer, literalWords(query))
		if err != nil {
			return result, err
		}
		stream.query = stream.matcher.query
	}
	result.Page, err = packPage(c, ReadEnvelopeBytes, stream.next, func(p Page[SearchHit]) any { return SearchPage{Page: p} })
	return result, err
}

type searchSource struct {
	position rankedSearchPosition
	text     string
	spans    []byteRange
}
type searchStream struct {
	l         *Ledger
	q         queryer
	cursor    readCursor
	query     string
	options   SearchOptions
	cached    *searchSource
	tokenizer *searchTokenizer
	matcher   *searchPhrases
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
	spans, err := s.matcher.spans(s.l.ctx, s.tokenizer, original)
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
	src.spans, err = s.matcher.spans(s.l.ctx, s.tokenizer, src.text)
	if err != nil {
		return nil, err
	}
	s.cached = src
	return src, nil
}

// searchPhrases is an Aho-Corasick automaton over SQLite's normalized tokens.
// Literal fields are phrases joined with AND by FTS5. Ranking already establishes
// that all phrases occur; here we find their union. For matches ending at the
// same token, the longest phrase contains every shorter match.
type searchPhraseNode struct {
	next   map[string]int
	fail   int
	length int
}
type searchPhrases struct {
	query   string
	nodes   []searchPhraseNode
	longest int
}

func newSearchPhrases(ctx context.Context, tokenizer *searchTokenizer, words []string) (*searchPhrases, error) {
	p := &searchPhrases{nodes: []searchPhraseNode{{next: map[string]int{}}}}
	queryWords := []string{}
	for _, word := range words {
		state, length := 0, 0
		err := tokenizer.visit(ctx, word, sqlite3.FTS5_TOKENIZE_QUERY, func(token string, _, _ int) {
			child, ok := p.nodes[state].next[token]
			if !ok {
				child = len(p.nodes)
				p.nodes[state].next[token] = child
				p.nodes = append(p.nodes, searchPhraseNode{next: map[string]int{}})
			}
			state = child
			length++
		})
		if err != nil {
			return nil, err
		}
		// Only SQLite decides whether a literal field contains a token. Its
		// unicode61 Unicode version differs from Go's current character tables.
		// Omit zero-token punctuation fields without dropping SQLite tokens.
		if length > 0 {
			queryWords = append(queryWords, word)
		}
		p.nodes[state].length = max(p.nodes[state].length, length)
		p.longest = max(p.longest, length)
	}
	p.query = literalQuery(strings.Join(queryWords, " "))
	queue := []int{0}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for token, child := range p.nodes[state].next {
			queue = append(queue, child)
			if state == 0 {
				continue
			}
			fallback := p.nodes[state].fail
			for fallback != 0 && p.nodes[fallback].next[token] == 0 {
				fallback = p.nodes[fallback].fail
			}
			p.nodes[child].fail = p.nodes[fallback].next[token]
			p.nodes[child].length = max(p.nodes[child].length, p.nodes[p.nodes[child].fail].length)
		}
	}
	return p, nil
}
func (p *searchPhrases) spans(ctx context.Context, tokenizer *searchTokenizer, text string) ([]byteRange, error) {
	spans := []byteRange{}
	if p.longest == 0 {
		return spans, nil
	}
	starts := make([]int, p.longest)
	state, position := 0, 0
	err := tokenizer.visit(ctx, text, sqlite3.FTS5_TOKENIZE_DOCUMENT, func(token string, start, end int) {
		starts[position%len(starts)] = start
		for state != 0 && p.nodes[state].next[token] == 0 {
			state = p.nodes[state].fail
		}
		state = p.nodes[state].next[token]
		if length := p.nodes[state].length; length > 0 {
			span := byteRange{starts[(position-length+1)%len(starts)], end}
			// Overlap, not adjacency, matches FTS5 highlight's shared-token union.
			// A later longer phrase may subsume several prior shorter spans.
			for len(spans) > 0 && span.start < spans[len(spans)-1].end {
				span.start = min(span.start, spans[len(spans)-1].start)
				spans = spans[:len(spans)-1]
			}
			spans = append(spans, span)
		}
		position++
	})
	return spans, err
}

// The driver exposes FTS5's documented API through its translated SQLite C ABI,
// but has no database/sql tokenizer API and does not compile fts3tokenize.
// This bridge owns a separate in-memory connection solely to obtain unicode61.
// It never accesses a stored ledger connection or private driver struct fields.
// Keep its tokenizer configuration identical to source_fts and entry_fts.
//
// modernc v1.44.2 represents C pointers/function pointers as uintptr. The only
// pointer reinterpretation below is for SQLite/TLS-owned memory or declared Go
// callback functions, using the same ABI as the driver. A numeric callback handle
// prevents any Go heap pointer from crossing that ABI. Each search owns its TLS,
// connection and tokenizer until synchronous callbacks finish. All supported
// targets (darwin/arm64, linux/arm64, linux/amd64) use this 64-bit ABI.
type searchTokenizer struct {
	tls      *libc.TLS
	db       uintptr
	instance uintptr
	module   sqlite3.Tfts5_tokenizer
}
type searchTokenizerSetup struct {
	db, statement, api, context, instance uintptr
	module                                sqlite3.Tfts5_tokenizer
}

// SQLite returns addresses into its translated C allocator, outside Go's heap.
// Reinterpret the address value as a pointer without pretending it is a Go
// object or permitting pointer arithmetic/lifetimes outside the owning call.
func searchNativePointer[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address))
}
func newSearchTokenizer() (_ *searchTokenizer, err error) {
	t := &searchTokenizer{tls: libc.NewTLS()}
	defer func() {
		if err != nil {
			t.close()
		}
	}()
	size := int(unsafe.Sizeof(searchTokenizerSetup{}))
	memory := t.tls.Alloc(size)
	defer t.tls.Free(size)
	setup := searchNativePointer[searchTokenizerSetup](memory)
	*setup = searchTokenizerSetup{}
	literals := []string{":memory:", "SELECT fts5(?)", "fts5_api_ptr", "unicode61"}
	cstrings := make([]uintptr, len(literals))
	defer func() {
		for _, p := range cstrings {
			libc.Xfree(t.tls, p)
		}
	}()
	for i, text := range literals {
		cstrings[i], err = libc.CString(text)
		if err != nil {
			return nil, err
		}
	}
	checkCode := func(stage string, code int32) error {
		if code != sqlite3.SQLITE_OK {
			return fmt.Errorf("search tokenizer %s: SQLite error %d", stage, code)
		}
		return nil
	}
	code := sqlite3.Xsqlite3_open_v2(t.tls, cstrings[0], uintptr(unsafe.Pointer(&setup.db)), sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, 0)
	t.db = setup.db
	if err = checkCode("open", code); err != nil {
		return nil, err
	}
	if err = checkCode("prepare", sqlite3.Xsqlite3_prepare_v2(t.tls, t.db, cstrings[1], -1, uintptr(unsafe.Pointer(&setup.statement)), 0)); err != nil {
		return nil, err
	}
	defer func() {
		if setup.statement != 0 {
			sqlite3.Xsqlite3_finalize(t.tls, setup.statement)
		}
	}()
	if err = checkCode("bind API", sqlite3.Xsqlite3_bind_pointer(t.tls, setup.statement, 1, uintptr(unsafe.Pointer(&setup.api)), cstrings[2], 0)); err != nil {
		return nil, err
	}
	if code = sqlite3.Xsqlite3_step(t.tls, setup.statement); code != sqlite3.SQLITE_ROW {
		return nil, fmt.Errorf("search tokenizer API: SQLite error %d", code)
	}
	if setup.api == 0 {
		return nil, fmt.Errorf("search tokenizer FTS5 API unavailable")
	}
	api := searchNativePointer[sqlite3.Tfts5_api](setup.api)
	if api.FiVersion < 2 || api.FxFindTokenizer == 0 {
		return nil, fmt.Errorf("search tokenizer requires FTS5 API version 2")
	}
	find := *(*func(*libc.TLS, uintptr, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&api.FxFindTokenizer))
	if err = checkCode("find unicode61", find(t.tls, setup.api, cstrings[3], uintptr(unsafe.Pointer(&setup.context)), uintptr(unsafe.Pointer(&setup.module)))); err != nil {
		return nil, err
	}
	t.module = setup.module
	if t.module.FxCreate == 0 || t.module.FxDelete == 0 || t.module.FxTokenize == 0 {
		return nil, fmt.Errorf("search tokenizer methods unavailable")
	}
	create := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&t.module.FxCreate))
	code = create(t.tls, setup.context, 0, 0, uintptr(unsafe.Pointer(&setup.instance)))
	t.instance = setup.instance
	if err = checkCode("create unicode61", code); err != nil {
		return nil, err
	}
	return t, nil
}
func (t *searchTokenizer) close() {
	if t.instance != 0 {
		destroy := *(*func(*libc.TLS, uintptr))(unsafe.Pointer(&t.module.FxDelete))
		destroy(t.tls, t.instance)
	}
	if t.db != 0 {
		sqlite3.Xsqlite3_close(t.tls, t.db)
	}
	t.tls.Close()
}

type searchTokenVisitor struct {
	ctx         context.Context
	input       string
	previousEnd int
	visit       func(string, int, int)
	err         error
}

var searchTokenVisitors sync.Map
var searchTokenHandle atomic.Uint64

func searchTokenCallback(_ *libc.TLS, handle uintptr, flags int32, token uintptr, n, start, end int32) int32 {
	value, ok := searchTokenVisitors.Load(handle)
	if !ok {
		return sqlite3.SQLITE_ABORT
	}
	visitor := value.(*searchTokenVisitor)
	if visitor.err = visitor.ctx.Err(); visitor.err != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if flags != 0 || n <= 0 || start < int32(visitor.previousEnd) || end <= start || int(end) > len(visitor.input) || !utf8.ValidString(visitor.input[start:end]) {
		visitor.err = fmt.Errorf("search tokenizer returned invalid Unicode61 offsets")
		return sqlite3.SQLITE_ERROR
	}
	visitor.previousEnd = int(end)
	// FTS5 clamps normalized query/index keys after tokenization, by bytes.
	// The capped key may end within a UTF-8 rune; only this private comparison
	// key is truncated. Original source offsets and evidence remain complete.
	n = min(n, int32(sqlite3.FTS5_MAX_TOKEN_SIZE))
	visitor.visit(string(unsafe.Slice(searchNativePointer[byte](token), n)), int(start), int(end))
	return sqlite3.SQLITE_OK
}
func (t *searchTokenizer) visit(ctx context.Context, text string, mode int32, visit func(string, int, int)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	input, err := libc.CString(text)
	if err != nil {
		return err
	}
	defer libc.Xfree(t.tls, input)
	visitor := &searchTokenVisitor{ctx: ctx, input: text, visit: visit}
	handle := uintptr(searchTokenHandle.Add(1))
	searchTokenVisitors.Store(handle, visitor)
	defer searchTokenVisitors.Delete(handle)
	callback := searchTokenCallback
	callbackPointer := *(*uintptr)(unsafe.Pointer(&callback))
	tokenize := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32)(unsafe.Pointer(&t.module.FxTokenize))
	code := tokenize(t.tls, t.instance, handle, mode, input, int32(len(text)), callbackPointer)
	if visitor.err != nil {
		return visitor.err
	}
	if code != sqlite3.SQLITE_OK {
		return fmt.Errorf("search tokenizer failed: SQLite error %d", code)
	}
	return nil
}
