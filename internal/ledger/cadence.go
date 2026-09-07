package ledger

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CompleteRootTurn records a distinct real user turn once. The database's
// immediate transaction mode keeps its ordinal and source anchors atomic.
func (l *Ledger) CompleteRootTurn(rootID string) (int64, error) {
	if strings.TrimSpace(rootID) == "" || !utf8.ValidString(rootID) {
		return 0, fmt.Errorf("root turn identity must be nonempty UTF-8")
	}
	tx, err := l.db.BeginTx(l.ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var completed sql.NullInt64
	err = tx.QueryRowContext(l.ctx, `SELECT completed_ordinal FROM root_turns WHERE id=?`, rootID).Scan(&completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if completed.Valid {
		return completed.Int64, tx.Commit()
	}
	value, err := meta(l.ctx, tx, "root_completed_ordinal")
	if err != nil {
		return 0, err
	}
	ordinal, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	ordinal++
	if _, err = tx.ExecContext(l.ctx, `INSERT INTO root_turns(id,completed_ordinal) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET completed_ordinal=excluded.completed_ordinal`, rootID, ordinal); err != nil {
		return 0, err
	}
	if err = setMeta(l.ctx, tx, "root_completed_ordinal", strconv.FormatInt(ordinal, 10)); err != nil {
		return 0, err
	}
	// A missing local anchor includes interrupted roots and imported provenance,
	// even when a round-trip import has the destination origin_session again.
	if _, err = tx.ExecContext(l.ctx, `UPDATE sources SET local_origin_ordinal=? WHERE local_origin_ordinal IS NULL AND EXISTS(SELECT 1 FROM evidence_units WHERE source_id=sources.id AND review_state='pending')`, ordinal); err != nil {
		return 0, err
	}
	return ordinal, tx.Commit()
}

// CapturePendingDebt pins the evidence boundary and bytes in one read snapshot.
func (l *Ledger) CapturePendingDebt() (PendingDebt, error) {
	debt := PendingDebt{}
	conn, close, err := l.readSnapshot()
	if err != nil {
		return debt, err
	}
	defer close()
	err = conn.QueryRowContext(l.ctx, `SELECT COALESCE(MAX(seq),0),COALESCE(SUM(CASE WHEN review_state='pending' THEN end_byte-start_byte ELSE 0 END),0) FROM evidence_units`).Scan(&debt.ThroughSeq, &debt.Bytes)
	return debt, err
}

// PendingDebtAfterRoot excludes appended final text and debt resolved by a
// concurrent checkpoint. Review state and local age come from one snapshot.
func (l *Ledger) PendingDebtAfterRoot(debt PendingDebt) (PendingDebtStatus, error) {
	status := PendingDebtStatus{}
	conn, close, err := l.readSnapshot()
	if err != nil {
		return status, err
	}
	defer close()
	err = conn.QueryRowContext(l.ctx, `SELECT COALESCE(SUM(u.end_byte-u.start_byte),0),COALESCE(MAX(0,CAST((SELECT value FROM meta WHERE key='root_completed_ordinal') AS INTEGER)-MIN(s.local_origin_ordinal)),0) FROM evidence_units u JOIN sources s ON s.id=u.source_id WHERE u.review_state='pending' AND u.seq<=?`, debt.ThroughSeq).Scan(&status.Bytes, &status.OldestAgeTurns)
	return status, err
}

// ClaimStopContinuation publishes the exact owned prompt and parent together;
// losing callers never replace the winner's metadata.
func (l *Ledger) ClaimStopContinuation(rootID, prompt string) (bool, error) {
	if rootID == "" || prompt == "" || !utf8.ValidString(rootID) || !utf8.ValidString(prompt) {
		return false, fmt.Errorf("continuation requires a root identity and UTF-8 prompt")
	}
	tx, err := l.db.BeginTx(l.ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(l.ctx, `UPDATE root_turns SET continuation_claimed=1,continuation_prompt=? WHERE id=? AND continuation_claimed=0`, prompt, rootID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	association, err := json.Marshal(struct {
		Root   string `json:"root"`
		Prompt string `json:"prompt"`
	}{rootID, prompt})
	if err != nil {
		return false, err
	}
	for _, item := range [][2]string{{"stop_prompt", prompt}, {"stop_turn", rootID}, {"stop_continuation", string(association)}} {
		if err = setMeta(l.ctx, tx, item[0], item[1]); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
