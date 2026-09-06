package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

type byteRange struct{ start, end int }

// segmentText accounts for the JSON string quotes and each rune's escaped cost.
// Each source byte belongs to exactly one range; source text is stored only once.
func segmentText(text string) ([]byteRange, error) {
	ranges := []byteRange{}
	start, cost := 0, 2
	for offset, r := range text {
		size := utf8.RuneLen(r)
		escaped := size
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			escaped = 2
		default:
			if r < 0x20 {
				escaped = 6
			}
		}
		if cost+escaped > EvidenceTextJSONBytes {
			ranges = append(ranges, byteRange{start, offset})
			start = offset
			cost = 2
		}
		cost += escaped
	}
	if start < len(text) {
		ranges = append(ranges, byteRange{start, len(text)})
	}
	for _, span := range ranges {
		encoded, err := JSON(text[span.start:span.end])
		if err != nil {
			return nil, err
		}
		if len(encoded) > EvidenceTextJSONBytes {
			return nil, fmt.Errorf("evidence text exceeds JSON limit")
		}
	}
	return ranges, nil
}

func (l *Ledger) CaptureV2(input CaptureInput) (CaptureReceipt, error) {
	receipt := CaptureReceipt{}
	if input.Kind != "user" && input.Kind != "assistant" && input.Kind != "tool" {
		return receipt, fmt.Errorf("invalid source kind %q", input.Kind)
	}
	if !utf8.ValidString(input.Text) || len(input.Text) == 0 || utf8.RuneCountInString(input.Text) > 1000000 {
		return receipt, fmt.Errorf("text must contain 1..=1000000 Unicode characters")
	}
	if !utf8.ValidString(input.Key) || !utf8.ValidString(input.RootTurnID) {
		return receipt, fmt.Errorf("source metadata must be valid UTF-8")
	}
	if input.OriginCompletionOrdinal != nil && *input.OriginCompletionOrdinal < 0 {
		return receipt, fmt.Errorf("origin completion ordinal must be nonnegative")
	}
	text := Redact(input.Text)
	id, err := identity("s-", []any{l.session, input.Kind, input.Key, text})
	if err != nil {
		return receipt, err
	}
	ranges, err := segmentText(text)
	if err != nil {
		return receipt, err
	}
	tx, err := l.db.BeginTx(l.ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	old, err := source(l.ctx, tx, id)
	if err == nil {
		var origin, key string
		if err = tx.QueryRowContext(l.ctx, `SELECT origin_session,origin_key FROM sources WHERE id=?`, id).Scan(&origin, &key); err != nil {
			return receipt, err
		}
		if old.ID != id || old.Kind != input.Kind || old.Text != text || origin != l.session || key != input.Key {
			return receipt, fmt.Errorf("source identity collision")
		}
		receipt.SourceID = id
		if err = tx.QueryRowContext(l.ctx, `SELECT COUNT(*) FROM evidence_units WHERE source_id=?`, id).Scan(&receipt.UnitCount); err != nil {
			return receipt, err
		}
		if err = tx.QueryRowContext(l.ctx, `SELECT id FROM evidence_units WHERE source_id=? ORDER BY seq LIMIT 1`, id).Scan(&receipt.FirstUnitID); err != nil {
			return receipt, err
		}
		return receipt, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	record := Source{ID: id, Kind: input.Kind, Timestamp: time.Now().UTC().Format(time.RFC3339), Text: text, Truncated: input.SourceIncomplete}
	body, err := JSON(record)
	if err != nil {
		return receipt, err
	}
	// Completion provenance supplied by a host never fabricates local progress.
	// Unknown roots remain unanchored until the cadence owner records a boundary.
	_, err = tx.ExecContext(l.ctx, `INSERT INTO sources(id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete)
 VALUES(?,?,?,?,?,(SELECT completed_ordinal FROM root_turns WHERE id=?),?,?)`, id, string(body), l.session, input.Key, input.RootTurnID, input.RootTurnID, input.OriginCompletionOrdinal, input.SourceIncomplete)
	if err != nil {
		return receipt, err
	}
	receipt.SourceID = id
	receipt.UnitCount = int64(len(ranges))
	for i, span := range ranges {
		unitID, err := identity("e-", []any{id, span.start, span.end})
		if err != nil {
			return CaptureReceipt{}, err
		}
		if _, err = tx.ExecContext(l.ctx, `INSERT INTO evidence_units(id,source_id,start_byte,end_byte) VALUES(?,?,?,?)`, unitID, id, span.start, span.end); err != nil {
			return CaptureReceipt{}, err
		}
		if i == 0 {
			receipt.FirstUnitID = unitID
		}
	}
	if _, err = tx.ExecContext(l.ctx, `UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='search_generation'`); err != nil {
		return CaptureReceipt{}, err
	}
	return receipt, tx.Commit()
}

// Capture temporarily supports existing adapter callers during the v2 rollout.
func (l *Ledger) Capture(kind, text, key string) (string, error) {
	receipt, err := l.CaptureV2(CaptureInput{Kind: kind, Text: text, Key: key})
	return receipt.SourceID, err
}
