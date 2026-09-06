package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Entries is retained only for serialized checkpoint fixture cleanup. Model-facing
// callers use ReadEntries; this method is not a protocol response.
func (l *Ledger) Entries(all bool) ([]Entry, error) {
	query := "SELECT seq,body,active,retirement FROM entries"
	if !all {
		query += " WHERE active=1"
	}
	query += " ORDER BY seq"
	rows, err := l.db.QueryContext(l.ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		e, err := readEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
func supportIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, fmt.Errorf("support must contain 1..=100 ids")
	}
	ids = slices.Clone(ids)
	sort.Strings(ids)
	return slices.Compact(ids), nil
}
func importanceRank(importance string) int {
	switch importance {
	case "low":
		return 0
	case "medium":
		return 1
	case "high":
		return 2
	case "critical":
		return 3
	default:
		return -1
	}
}
func insertEntry(ctx context.Context, q queryer, kind, timestamp, importance, text string, support []string) (string, error) {
	text, err := cleanText(text, 2000)
	if err != nil {
		return "", err
	}
	text = Redact(text)
	if importance == "" {
		importance = "medium"
	}
	if importanceRank(importance) < 0 {
		return "", fmt.Errorf("invalid importance %q", importance)
	}
	prefix := "o-"
	if kind == "reflection" {
		prefix = "r-"
	}
	id, err := identity(prefix, []any{text, support, importance})
	if err != nil {
		return "", err
	}
	record := Entry{ID: id, Kind: kind, Timestamp: timestamp, Importance: importance, Text: text, Support: support, Active: true}
	body, err := JSON(record)
	if err != nil {
		return "", err
	}
	_, err = q.ExecContext(ctx, "INSERT OR IGNORE INTO entries(id,body,effective_priority) VALUES (?,?,?)", id, string(body), importanceRank(importance))
	return id, err
}

// Recall is retained for the unconverted checkpoint fixture. Use ReadRecall
// for bounded, exact evidence inspection.
func (l *Ledger) Recall(id string) (Recall, error) {
	result := Recall{Sources: []Source{}}
	if strings.HasPrefix(id, "e-") {
		if err := l.db.QueryRowContext(l.ctx, "SELECT source_id FROM evidence_units WHERE id=?", id).Scan(&id); err != nil {
			return result, err
		}
	}
	if strings.HasPrefix(id, "s-") {
		s, err := source(l.ctx, l.db, id)
		if err != nil {
			return result, err
		}
		result.Sources = append(result.Sources, s)
		return result, nil
	}
	record, err := entry(l.ctx, l.db, id)
	if err != nil {
		return result, err
	}
	result.Entry = &record
	observations := []Entry{record}
	if record.Kind == "reflection" {
		observations = []Entry{}
		for _, id := range record.Support {
			e, err := entry(l.ctx, l.db, id)
			if err != nil {
				return result, err
			}
			observations = append(observations, e)
		}
		sort.Slice(observations, func(i, j int) bool { return observations[i].Seq < observations[j].Seq })
		result.Observations = observations
	}
	seen := map[string]bool{}
	for _, o := range observations {
		for _, id := range o.Support {
			var sourceID string
			if err := l.db.QueryRowContext(l.ctx, "SELECT source_id FROM evidence_units WHERE id=?", id).Scan(&sourceID); err != nil {
				return result, err
			}
			id = sourceID
			if seen[id] {
				continue
			}
			seen[id] = true
			s, err := source(l.ctx, l.db, id)
			if err != nil {
				return result, err
			}
			result.Sources = append(result.Sources, s)
		}
	}
	sort.Slice(result.Sources, func(i, j int) bool { return result.Sources[i].Seq < result.Sources[j].Seq })
	return result, nil
}
func (l *Ledger) Status() (Status, error) {
	status := Status{Session: l.session, Database: l.Path}
	conn, err := l.db.Conn(l.ctx)
	if err != nil {
		return status, err
	}
	defer conn.Close()
	// Explicit DEFERRED gives a coherent read snapshot without claiming a writer.
	if _, err = conn.ExecContext(l.ctx, "BEGIN DEFERRED"); err != nil {
		return status, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if status.Through, err = cursor(l.ctx, conn); err != nil {
		return status, err
	}
	paused, err := meta(l.ctx, conn, "paused")
	if err != nil {
		return status, err
	}
	status.Paused = paused == "1"
	if status.LastCheckpointAt, err = meta(l.ctx, conn, "last_checkpoint_at"); err != nil {
		return status, err
	}
	parent, err := meta(l.ctx, conn, "forked_from")
	if err != nil {
		return status, err
	}
	if parent != "" {
		store, err := meta(l.ctx, conn, "forked_from_store")
		if err != nil {
			return status, err
		}
		status.ImportedFrom = &SessionReference{Store: store, Session: parent}
	}
	revision, err := meta(l.ctx, conn, "revision")
	if err != nil {
		return status, err
	}
	if status.Revision, err = strconv.ParseInt(revision, 10, 64); err != nil {
		return status, err
	}
	if status.Coverage, err = coverageWithinTx(l.ctx, conn); err != nil {
		return status, err
	}
	status.UnitCount = status.Coverage.Pending.Units + status.Coverage.Reviewed.Units + status.Coverage.Deferred.Units
	if err = conn.QueryRowContext(l.ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(json_extract(body,'$.text') AS BLOB))),0) FROM sources`).Scan(&status.SourceCount, &status.StoredSourceBytes); err != nil {
		return status, err
	}
	if status.PendingSources, status.PendingChars, err = pendingCharacterCounts(l.ctx, conn); err != nil {
		return status, err
	}
	if err = conn.QueryRowContext(l.ctx, `SELECT COALESCE(SUM(json_extract(body,'$.kind')='observation'),0),COALESCE(SUM(json_extract(body,'$.kind')='reflection'),0) FROM entries WHERE active=1`).Scan(&status.Active.Observations, &status.Active.Reflections); err != nil {
		return status, err
	}
	if err = conn.QueryRowContext(l.ctx, `SELECT COALESCE(MAX(MAX(0,CAST((SELECT value FROM meta WHERE key='root_completed_ordinal') AS INTEGER)-s.local_origin_ordinal)),0)
 FROM sources s WHERE EXISTS(SELECT 1 FROM evidence_units u WHERE u.source_id=s.id AND u.review_state='pending')`).Scan(&status.OldestPendingAgeTurns); err != nil {
		return status, err
	}
	var metadata WorkingStateMetadata
	err = conn.QueryRowContext(l.ctx, `SELECT revision,updated_at,MAX(0,CAST((SELECT value FROM meta WHERE key='root_completed_ordinal') AS INTEGER)-local_origin_ordinal) FROM working_state WHERE singleton=1`).Scan(&metadata.Revision, &metadata.UpdatedAt, &metadata.AgeRootTurns)
	if err == nil {
		status.WorkingState = &metadata
	} else if !errors.Is(err, sql.ErrNoRows) {
		return status, err
	}
	status.EstimatedPendingTokens = (status.PendingChars + 3) / 4
	_, err = conn.ExecContext(l.ctx, "COMMIT")
	return status, err
}

// These legacy character counts are separate from v2 byte coverage. Aggregating
// ranges in a subquery decodes each pending source once, then counts only its
// pending ranges. Go's rune count includes embedded NUL (SQLite length does not).
func pendingCharacterCounts(ctx context.Context, q queryer) (sources, chars int64, err error) {
	rows, err := q.QueryContext(ctx, `SELECT json_extract(s.body,'$.text'),
 (SELECT json_group_array(json_array(start_byte,end_byte)) FROM evidence_units WHERE source_id=s.id AND review_state='pending')
 FROM sources s WHERE EXISTS(SELECT 1 FROM evidence_units WHERE source_id=s.id AND review_state='pending')`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var text, encodedRanges string
		if err = rows.Scan(&text, &encodedRanges); err != nil {
			return 0, 0, err
		}
		var ranges [][2]int64
		if err = json.Unmarshal([]byte(encodedRanges), &ranges); err != nil {
			return 0, 0, err
		}
		for _, span := range ranges {
			if span[0] < 0 || span[1] <= span[0] || span[1] > int64(len(text)) {
				return 0, 0, fmt.Errorf("invalid evidence range in pending source")
			}
			chars += int64(utf8.RuneCountInString(text[span[0]:span[1]]))
		}
		sources++
	}
	return sources, chars, rows.Err()
}
