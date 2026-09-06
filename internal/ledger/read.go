package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const evidenceSelect = `SELECT u.seq,u.id,u.source_id,json_extract(s.body,'$.kind'),json_extract(s.body,'$.timestamp'),u.start_byte,u.end_byte,
 CAST(substr(CAST(json_extract(s.body,'$.text') AS BLOB),u.start_byte+1,u.end_byte-u.start_byte) AS TEXT),s.source_incomplete,u.review_state,u.deferral_reason
 FROM evidence_units u JOIN sources s ON s.id=u.source_id`

func scanEvidence(row scanner) (EvidenceUnit, error) {
	var u EvidenceUnit
	err := row.Scan(&u.Seq, &u.ID, &u.SourceID, &u.Kind, &u.Timestamp, &u.StartByte, &u.EndByte, &u.Text, &u.SourceIncomplete, &u.ReviewState, &u.DeferralReason)
	return u, err
}
func (l *Ledger) ReadPending(token string) (PendingPage, error) {
	result := PendingPage{}
	q, close, err := l.readSnapshot()
	if err != nil {
		return result, err
	}
	defer close()
	c, err := l.pageCursor(q, token, "pending", "", false)
	if err != nil {
		return result, err
	}
	if c.Position.Record != 0 || c.Position.Offset != 0 || (c.Position.Phase != "" && c.Position.Phase != "evidence") {
		return result, fmt.Errorf("invalid pending position; restart pagination")
	}
	result.Revision = c.MemoryRevision
	if result.Through, err = cursor(l.ctx, q); err != nil {
		return result, err
	}
	if result.Coverage, err = coverageWithinTx(l.ctx, q); err != nil {
		return result, err
	}
	next := func(p readPosition) (EvidenceUnit, readPosition, bool, error) {
		u, err := scanEvidence(q.QueryRowContext(l.ctx, evidenceSelect+` WHERE u.review_state='pending' AND s.seq<=? AND u.seq>? ORDER BY u.seq LIMIT 1`, c.SourceHighWater, p.Evidence))
		if errors.Is(err, sql.ErrNoRows) {
			return u, p, false, nil
		}
		p.Phase = "evidence"
		p.Evidence = u.Seq
		return u, p, err == nil, err
	}
	result.Page, err = packPage(c, MaxPendingPageItems, next, func(p Page[EvidenceUnit]) any { r := result; r.Page = p; return r })
	return result, err
}
func (l *Ledger) ReadEntries(token string) (InspectionPage, error) {
	result := InspectionPage{}
	q, close, err := l.readSnapshot()
	if err != nil {
		return result, err
	}
	defer close()
	c, err := l.pageCursor(q, token, "entries", "", false)
	if err != nil {
		return result, err
	}
	stream, err := l.inspection(q, c, "")
	if err != nil {
		return result, err
	}
	result.Page, err = packPage(c, ReadEnvelopeBytes, stream.next, func(p Page[InspectionItem]) any { return InspectionPage{Page: p} })
	return result, err
}
func (l *Ledger) ReadRecall(id, token string) (RecallPage, error) {
	result := RecallPage{ID: id}
	if id == "" {
		return result, fmt.Errorf("recall requires a record or evidence ID")
	}
	q, close, err := l.readSnapshot()
	if err != nil {
		return result, err
	}
	defer close()
	c, err := l.pageCursor(q, token, "recall", id, false)
	if err != nil {
		return result, err
	}
	stream, err := l.inspection(q, c, id)
	if err != nil {
		return result, err
	}
	result.Kind = stream.kind
	result.Page, err = packPage(c, ReadEnvelopeBytes, stream.next, func(p Page[InspectionItem]) any { r := result; r.Page = p; return r })
	return result, err
}

type inspectionStream struct {
	l          *Ledger
	q          queryer
	cursor     readCursor
	kind       string
	records    []Entry
	sources    []string
	evidenceID string
	all        bool
}

func (l *Ledger) inspection(q queryer, c readCursor, id string) (*inspectionStream, error) {
	s := &inspectionStream{l: l, q: q, cursor: c, all: id == ""}
	if id == "" {
		rows, err := q.QueryContext(l.ctx, `SELECT seq,body,active,retirement FROM entries ORDER BY seq`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			e, err := readEntry(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			s.records = append(s.records, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	} else if strings.HasPrefix(id, "s-") {
		var seq int64
		if err := q.QueryRowContext(l.ctx, `SELECT seq FROM sources WHERE id=? AND seq<=?`, id, c.SourceHighWater).Scan(&seq); err != nil {
			return nil, err
		}
		s.sources = []string{id}
		s.kind = "source"
	} else if strings.HasPrefix(id, "e-") {
		var seq int64
		if err := q.QueryRowContext(l.ctx, `SELECT u.seq FROM evidence_units u JOIN sources s ON s.id=u.source_id WHERE u.id=? AND s.seq<=?`, id, c.SourceHighWater).Scan(&seq); err != nil {
			return nil, err
		}
		s.evidenceID = id
		s.kind = "evidence"
	} else {
		e, err := entry(l.ctx, q, id)
		if err != nil {
			return nil, err
		}
		s.kind = e.Kind
		s.records = []Entry{e}
		if e.Kind == "reflection" {
			for _, support := range e.Support {
				o, err := entry(l.ctx, q, support)
				if err != nil {
					return nil, err
				}
				s.records = append(s.records, o)
			}
			sort.Slice(s.records[1:], func(i, j int) bool { return s.records[i+1].Seq < s.records[j+1].Seq })
		}
	}
	if id != "" && len(s.records) > 0 {
		seen := map[string]bool{}
		for _, e := range s.records {
			if e.Kind != "observation" {
				continue
			}
			for _, support := range e.Support {
				var sourceID string
				if err := q.QueryRowContext(l.ctx, `SELECT source_id FROM evidence_units WHERE id=?`, support).Scan(&sourceID); err != nil {
					return nil, err
				}
				if !seen[sourceID] {
					seen[sourceID] = true
					s.sources = append(s.sources, sourceID)
				}
			}
		}
	}
	if c.Position.Record > int64(len(s.records)) {
		return nil, fmt.Errorf("invalid record position; restart pagination")
	}
	return s, nil
}
func (s *inspectionStream) next(p readPosition) (InspectionItem, readPosition, bool, error) {
	empty := InspectionItem{}
	for p.Record < int64(len(s.records)) {
		e := s.records[p.Record]
		switch p.Phase {
		case "", "header":
			var priority int
			if err := s.q.QueryRowContext(s.l.ctx, `SELECT effective_priority FROM entries WHERE id=?`, e.ID).Scan(&priority); err != nil {
				return empty, p, false, err
			}
			h := RecordHeader{ID: e.ID, Kind: e.Kind, Seq: e.Seq, Timestamp: e.Timestamp, Active: e.Active, Importance: e.Importance, EffectiveImportance: priorityName(priority)}
			p.Phase = "text"
			p.Offset = 0
			return InspectionItem{Header: &h}, p, true, nil
		case "text", "reason":
			text, field := e.Text, "text"
			next := "reason"
			if p.Phase == "reason" {
				text = ""
				field = "retirement.reason"
				next = "support"
				if e.Retirement != nil {
					text = e.Retirement.Reason
				}
			}
			if p.Offset > int64(len(text)) || p.Offset < int64(len(text)) && !utf8.RuneStart(text[p.Offset]) {
				return empty, p, false, fmt.Errorf("invalid field position; restart pagination")
			}
			if p.Offset < int64(len(text)) {
				// Field pieces have the same escaped-text ceiling as evidence units; the
				// shared packer accounts for surrounding fields and actual cursor bytes.
				ranges, err := segmentText(text[p.Offset:])
				if err != nil {
					return empty, p, false, err
				}
				end := p.Offset + int64(ranges[0].end)
				f := FieldFragment{RecordID: e.ID, Field: field, Text: text[p.Offset:end], StartByte: p.Offset, EndByte: end, TotalBytes: int64(len(text))}
				p.Offset = end
				return InspectionItem{Field: &f}, p, true, nil
			}
			p.Phase = next
			p.Offset = 0
		case "support", "replacement":
			ids, relation := e.Support, "evidence"
			next := "replacement"
			if e.Kind == "reflection" {
				relation = "observation"
			}
			if p.Phase == "replacement" {
				ids = nil
				relation = "replacement"
				next = "header"
				if e.Retirement != nil {
					ids = e.Retirement.ReplacementIDs
				}
			}
			if p.Offset > int64(len(ids)) {
				return empty, p, false, fmt.Errorf("invalid relation position; restart pagination")
			}
			if p.Offset < int64(len(ids)) {
				r := SupportReference{RecordID: e.ID, Relation: relation, TargetID: ids[p.Offset], Ordinal: p.Offset}
				p.Offset++
				return InspectionItem{Support: &r}, p, true, nil
			}
			if p.Phase == "replacement" {
				p.Record++
			}
			p.Phase = next
			p.Offset = 0
		default:
			return empty, p, false, fmt.Errorf("invalid inspection position; restart pagination")
		}
	}
	p.Phase = "evidence"
	p.Offset = 0
	query := evidenceSelect + ` WHERE s.seq<=? AND u.seq>?`
	args := []any{s.cursor.SourceHighWater, p.Evidence}
	if s.evidenceID != "" {
		query += ` AND u.id=?`
		args = append(args, s.evidenceID)
	} else if s.all {
		query += ` AND EXISTS (SELECT 1 FROM entries e, json_each(e.body,'$.support') refs JOIN evidence_units cited ON cited.id=refs.value WHERE json_extract(e.body,'$.kind')='observation' AND cited.source_id=u.source_id)`
	} else {
		if len(s.sources) == 0 {
			return empty, p, false, nil
		}
		query += ` AND u.source_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(s.sources)), ",") + `)`
		for _, id := range s.sources {
			args = append(args, id)
		}
	}
	u, err := scanEvidence(s.q.QueryRowContext(s.l.ctx, query+` ORDER BY u.seq LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return empty, p, false, nil
	}
	if err != nil {
		return empty, p, false, err
	}
	p.Evidence = u.Seq
	return InspectionItem{Evidence: &u}, p, true, nil
}
func priorityName(rank int) string {
	switch rank {
	case 0:
		return "low"
	case 1:
		return "medium"
	case 2:
		return "high"
	case 3:
		return "critical"
	}
	return "unknown"
}
