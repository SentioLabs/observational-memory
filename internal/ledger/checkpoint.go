package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ApplyV2 commits explicit evidence coverage and memory as a single operation.
// The store's immediate transaction mode serializes competing revision checks.
func (l *Ledger) ApplyV2(cp CheckpointV2) (ReceiptV2, error) {
	receipt := ReceiptV2{Observations: []string{}, Reflections: []string{}, Retired: []string{}}
	digest, err := canonicalReceiptDigest(cp)
	if err != nil {
		return receipt, err
	}
	tx, err := l.db.BeginTx(l.ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var saved []byte
	err = tx.QueryRowContext(l.ctx, `SELECT body FROM checkpoint_receipts WHERE digest=?`, digest).Scan(&saved)
	if err == nil {
		err = json.Unmarshal(saved, &receipt)
		return receipt, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	through, err := cursor(l.ctx, tx)
	if err != nil {
		return receipt, err
	}
	revisionText, err := meta(l.ctx, tx, "revision")
	if err != nil {
		return receipt, err
	}
	revision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil {
		return receipt, err
	}
	if cp.ExpectedThrough != through || cp.ExpectedRevision != revision {
		return receipt, fmt.Errorf("stale checkpoint cursor or revision; reread pending")
	}
	if err = validateOperations(cp); err != nil {
		return receipt, err
	}
	if err = l.applyReviewStates(tx, cp); err != nil {
		return receipt, err
	}
	if receipt.Observations, err = l.insertObservations(tx, cp.Observations); err != nil {
		return receipt, err
	}
	if receipt.Reflections, err = l.insertReflections(tx, cp.Reflections); err != nil {
		return receipt, err
	}
	if receipt.Retired, err = l.propagateReplacementFloors(tx, cp.Retire, receipt.Reflections); err != nil {
		return receipt, err
	}
	receipt.Revision = revision + 1
	now := time.Now().UTC().Format(time.RFC3339)
	if cp.WorkingState != nil {
		body, err := l.validateWorkingState(tx, *cp.WorkingState)
		if err != nil {
			return receipt, err
		}
		_, err = tx.ExecContext(l.ctx, `INSERT INTO working_state(singleton,body,revision,updated_at,local_origin_ordinal) VALUES(1,?,?,?,CAST((SELECT value FROM meta WHERE key='root_completed_ordinal') AS INTEGER)) ON CONFLICT(singleton) DO UPDATE SET body=excluded.body,revision=excluded.revision,updated_at=excluded.updated_at,local_origin_ordinal=excluded.local_origin_ordinal`, string(body), receipt.Revision, now)
		if err != nil {
			return receipt, err
		}
	}
	if receipt.Through, err = resolvedCursor(l.ctx, tx); err != nil {
		return receipt, err
	}
	if receipt.Coverage, err = coverageWithinTx(l.ctx, tx); err != nil {
		return receipt, err
	}
	for key, value := range map[string]string{"cursor": strconv.FormatInt(receipt.Through, 10), "revision": strconv.FormatInt(receipt.Revision, 10), "last_checkpoint_at": now} {
		if err = setMeta(l.ctx, tx, key, value); err != nil {
			return receipt, err
		}
	}
	if len(cp.Observations)+len(cp.Reflections)+len(cp.Retire) > 0 {
		if _, err = tx.ExecContext(l.ctx, `UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='search_generation'`); err != nil {
			return receipt, err
		}
	}
	body, err := EncodeResponse(receipt)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(l.ctx, `INSERT INTO checkpoint_receipts(digest,body) VALUES(?,?)`, digest, body); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

func canonicalReceiptDigest(cp CheckpointV2) (string, error) {
	body, err := JSON(cp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
func validateOperations(cp CheckpointV2) error {
	count := len(cp.Acknowledge) + len(cp.DeferSources) + len(cp.ReviewDeferred) + len(cp.Observations) + len(cp.Reflections) + len(cp.Retire)
	if cp.WorkingState != nil {
		count++
	}
	if count > MaxCheckpointItems {
		return fmt.Errorf("checkpoint exceeds %d operations", MaxCheckpointItems)
	}
	seen := map[string]bool{}
	for _, ids := range [][]string{cp.Acknowledge, cp.ReviewDeferred} {
		for _, id := range ids {
			if !strings.HasPrefix(id, "e-") || seen[id] {
				return fmt.Errorf("invalid, duplicate or overlapping evidence operation %q", id)
			}
			seen[id] = true
		}
	}
	seen = map[string]bool{}
	for _, d := range cp.DeferSources {
		if seen[d.SourceID] {
			return fmt.Errorf("duplicate source deferral %q", d.SourceID)
		}
		seen[d.SourceID] = true
		if !utf8.ValidString(d.Reason) || strings.TrimSpace(d.Reason) == "" || len(d.Reason) > MaxDeferralReasonBytes || len(Redact(d.Reason)) > MaxDeferralReasonBytes {
			return fmt.Errorf("deferral reason must contain 1..=%d UTF-8 bytes", MaxDeferralReasonBytes)
		}
	}
	seen = map[string]bool{}
	for _, r := range cp.Retire {
		if seen[r.ID] {
			return fmt.Errorf("duplicate retirement %q", r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}
func (l *Ledger) applyReviewStates(q queryer, cp CheckpointV2) error {
	// Check deferred reviews against the original state, before prospective deferrals.
	for _, id := range cp.ReviewDeferred {
		var state string
		if err := q.QueryRowContext(l.ctx, `SELECT review_state FROM evidence_units WHERE id=?`, id).Scan(&state); err != nil {
			return fmt.Errorf("unknown evidence %s: %w", id, err)
		}
		if state != string(ReviewDeferred) {
			return fmt.Errorf("review_deferred requires previously deferred evidence")
		}
	}
	for _, d := range cp.DeferSources {
		var kind string
		if err := q.QueryRowContext(l.ctx, `SELECT json_extract(body,'$.kind') FROM sources WHERE id=?`, d.SourceID).Scan(&kind); err != nil {
			return fmt.Errorf("unknown source %s: %w", d.SourceID, err)
		}
		if kind != "tool" {
			return fmt.Errorf("only tool sources may be deferred")
		}
		for _, id := range append(slices.Clone(cp.Acknowledge), cp.ReviewDeferred...) {
			var sourceID, state string
			err := q.QueryRowContext(l.ctx, `SELECT source_id,review_state FROM evidence_units WHERE id=?`, id).Scan(&sourceID, &state)
			if err != nil {
				return fmt.Errorf("unknown evidence %s: %w", id, err)
			}
			if sourceID == d.SourceID && state == string(ReviewPending) {
				return fmt.Errorf("source deferral overlaps evidence operation")
			}
		}
		result, err := q.ExecContext(l.ctx, `UPDATE evidence_units SET review_state='deferred',deferral_reason=? WHERE source_id=? AND review_state='pending'`, Redact(d.Reason), d.SourceID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("source deferral requires pending units")
		}
	}
	rows, err := q.QueryContext(l.ctx, `SELECT id FROM evidence_units WHERE review_state='pending' ORDER BY seq LIMIT ?`, len(cp.Acknowledge))
	if err != nil {
		return err
	}
	prefix := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		prefix = append(prefix, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !slices.Equal(prefix, cp.Acknowledge) {
		return fmt.Errorf("acknowledge must be the exact oldest pending prefix")
	}
	for _, ids := range [][]string{cp.Acknowledge, cp.ReviewDeferred} {
		for _, id := range ids {
			if _, err = q.ExecContext(l.ctx, `UPDATE evidence_units SET review_state='reviewed' WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}
func (l *Ledger) evidenceSupport(q queryer, ids []string) ([]string, string, error) {
	support, err := supportIDs(ids)
	if err != nil {
		return nil, "", err
	}
	timestamp := ""
	for _, id := range support {
		var ts string
		err = q.QueryRowContext(l.ctx, `SELECT json_extract(s.body,'$.timestamp') FROM evidence_units u JOIN sources s ON s.id=u.source_id WHERE u.id=?`, id).Scan(&ts)
		if err != nil {
			return nil, "", fmt.Errorf("unknown evidence %s: %w", id, err)
		}
		if ts > timestamp {
			timestamp = ts
		}
	}
	return support, timestamp, nil
}
func (l *Ledger) insertObservations(q queryer, proposals []ObservationV2) ([]string, error) {
	result := []string{}
	for _, p := range proposals {
		support, timestamp, err := l.evidenceSupport(q, p.EvidenceIDs)
		if err != nil {
			return nil, err
		}
		id, err := insertEntry(l.ctx, q, "observation", timestamp, p.Importance, p.Text, support)
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}
func (l *Ledger) insertReflections(q queryer, proposals []Reflection) ([]string, error) {
	result := []string{}
	for _, p := range proposals {
		support, err := supportIDs(p.ObservationIDs)
		if err != nil {
			return nil, err
		}
		timestamp := ""
		floor := importanceRank("medium")
		for _, id := range support {
			e, err := entry(l.ctx, q, id)
			if err != nil {
				return nil, err
			}
			if e.Kind != "observation" || !e.Active {
				return nil, fmt.Errorf("reflection support must be active observations")
			}
			if e.Timestamp > timestamp {
				timestamp = e.Timestamp
			}
			var priority int
			if err = q.QueryRowContext(l.ctx, `SELECT effective_priority FROM entries WHERE id=?`, id).Scan(&priority); err != nil {
				return nil, err
			}
			floor = max(floor, priority)
		}
		id, err := insertEntry(l.ctx, q, "reflection", timestamp, "medium", p.Text, support)
		if err != nil {
			return nil, err
		}
		if _, err = q.ExecContext(l.ctx, `UPDATE entries SET effective_priority=MAX(effective_priority,?) WHERE id=?`, floor, id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}
func (l *Ledger) propagateReplacementFloors(q queryer, changes []Retirement, reflectionIDs []string) ([]string, error) {
	// New reflections inherit the final support floor, including support promoted
	// by this checkpoint. Each support precedes its reflection in entry order.
	supporting := map[string][]string{}
	for _, id := range reflectionIDs {
		reflection, err := entry(l.ctx, q, id)
		if err != nil {
			return nil, err
		}
		for _, observation := range reflection.Support {
			supporting[observation] = append(supporting[observation], id)
		}
	}
	prior := map[string]Entry{}
	validated := make([]Retirement, 0, len(changes))
	result := []string{}
	for _, change := range changes {
		old, err := entry(l.ctx, q, change.ID)
		if err != nil {
			return nil, err
		}
		if !old.Active {
			return nil, fmt.Errorf("retirement requires an active entry")
		}
		prior[old.ID] = old
		change.Reason, err = cleanText(change.Reason, 1000)
		if err != nil {
			return nil, err
		}
		change.Reason = Redact(change.Reason)
		replacements, err := supportIDs(change.ReplacementIDs)
		if err != nil {
			return nil, err
		}
		for _, id := range replacements {
			newer, err := entry(l.ctx, q, id)
			if err != nil {
				return nil, err
			}
			if !newer.Active || newer.Seq <= old.Seq {
				return nil, fmt.Errorf("replacement must be newer and active")
			}
			if newer.Kind != old.Kind && !(newer.Kind == "reflection" && slices.Contains(newer.Support, old.ID)) {
				return nil, fmt.Errorf("replacement must supersede the same kind or be a supporting reflection")
			}
		}
		change.ReplacementIDs = replacements
		validated = append(validated, change)
		result = append(result, change.ID)
	}
	// Validate all edges while entries are active, then carry each floor forward
	// in sequence order even when the request lists a replacement chain backwards.
	sort.Slice(validated, func(i, j int) bool { return prior[validated[i].ID].Seq < prior[validated[j].ID].Seq })
	for _, change := range validated {
		var floor int
		if err := q.QueryRowContext(l.ctx, `SELECT effective_priority FROM entries WHERE id=?`, change.ID).Scan(&floor); err != nil {
			return nil, err
		}
		for _, id := range change.ReplacementIDs {
			if _, err := q.ExecContext(l.ctx, `UPDATE entries SET effective_priority=MAX(effective_priority,?) WHERE id=?`, floor, id); err != nil {
				return nil, err
			}
			for _, reflectionID := range supporting[id] {
				if _, err := q.ExecContext(l.ctx, `UPDATE entries SET effective_priority=MAX(effective_priority,?) WHERE id=?`, floor, reflectionID); err != nil {
					return nil, err
				}
			}
		}
		body, err := JSON(change)
		if err != nil {
			return nil, err
		}
		if _, err = q.ExecContext(l.ctx, `UPDATE entries SET active=0,retirement=? WHERE id=?`, string(body), change.ID); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (l *Ledger) validateWorkingState(q queryer, state WorkingState) ([]byte, error) {
	// Validate the supplied encoded representation before normalization/redaction.
	body, err := JSON(state)
	if err != nil {
		return nil, err
	}
	if len(body) > WorkingStateBytes {
		return nil, fmt.Errorf("working state exceeds %d bytes", WorkingStateBytes)
	}
	validate := func(f WorkingFact) (WorkingFact, error) {
		text, err := cleanText(f.Text, WorkingStateBytes)
		if err != nil {
			return f, err
		}
		support, _, err := l.evidenceSupport(q, f.EvidenceIDs)
		if err != nil {
			return f, err
		}
		return WorkingFact{Text: Redact(text), EvidenceIDs: support}, nil
	}
	if state.Objective != nil {
		fact, err := validate(*state.Objective)
		if err != nil {
			return nil, err
		}
		state.Objective = &fact
	}
	groups := []*[]WorkingFact{&state.Constraints, &state.Completed, &state.Open, &state.Next}
	for _, group := range groups {
		*group = slices.Clone(*group)
		for i, f := range *group {
			validated, err := validate(f)
			if err != nil {
				return nil, err
			}
			(*group)[i] = validated
		}
	}
	body, err = JSON(state)
	if err != nil {
		return nil, err
	}
	if len(body) > WorkingStateBytes {
		return nil, fmt.Errorf("working state exceeds %d bytes", WorkingStateBytes)
	}
	return body, nil
}
func resolvedCursor(ctx context.Context, q queryer) (int64, error) {
	var through int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT MIN(seq)-1 FROM evidence_units WHERE review_state='pending'),(SELECT MAX(seq) FROM evidence_units),0)`).Scan(&through)
	return through, err
}
func coverageWithinTx(ctx context.Context, q queryer) (Coverage, error) {
	var coverage Coverage
	rows, err := q.QueryContext(ctx, `SELECT review_state,COUNT(*),COALESCE(SUM(end_byte-start_byte),0) FROM evidence_units GROUP BY review_state`)
	if err != nil {
		return coverage, err
	}
	defer rows.Close()
	for rows.Next() {
		var state ReviewState
		var count CoverageCount
		if err = rows.Scan(&state, &count.Units, &count.Bytes); err != nil {
			return coverage, err
		}
		switch state {
		case ReviewPending:
			coverage.Pending = count
		case ReviewReviewed:
			coverage.Reviewed = count
		case ReviewDeferred:
			coverage.Deferred = count
		}
	}
	return coverage, rows.Err()
}
