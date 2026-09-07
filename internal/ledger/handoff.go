package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"modernc.org/sqlite"
)

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// handoffBoundary permits test-scoped fault injection through the operation's
// context. Production contexts have no hook; no shared mutable state exists.
func handoffBoundary(ctx context.Context, stage string) {
	if hook, ok := ctx.(interface{ handoffBoundary(string) }); ok {
		hook.handoffBoundary(stage)
	}
}

// snapshot constructs a private, complete snapshot before its destination is
// exposed. No failure during backup or metadata preparation reserves a session.
func (l *Ledger) snapshot(destination string) (path string, status Status, cleanup func(), err error) {
	dir, err := os.MkdirTemp(l.store, ".snapshot-*")
	if err != nil {
		return "", status, nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	path = filepath.Join(dir, "memory.sqlite3")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", status, cleanup, err
	}
	if err = f.Close(); err != nil {
		return "", status, cleanup, err
	}
	conn, err := l.db.Conn(l.ctx)
	if err != nil {
		return "", status, cleanup, err
	}
	err = conn.Raw(func(raw any) error {
		api, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite backup API unavailable")
		}
		backup, err := api.NewBackup(fileURI(path))
		if err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			handoffBoundary(l.ctx, "backup")
			if err = l.ctx.Err(); err != nil {
				break
			}
			var more bool
			more, err = backup.Step(100)
			if err != nil || !more {
				break
			}
			if time.Now().After(deadline) {
				err = fmt.Errorf("snapshot exceeded two-second budget")
				break
			}
		}
		return errors.Join(err, backup.Finish())
	})
	err = errors.Join(err, conn.Close())
	if err != nil {
		return "", status, cleanup, err
	}
	copied, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		return "", status, cleanup, err
	}
	defer func() { err = errors.Join(err, copied.Close()) }()
	tx, err := copied.BeginTx(l.ctx, nil)
	if err != nil {
		return "", status, cleanup, err
	}
	defer tx.Rollback()
	// The backup includes base rows and their FTS indexes in one SQLite
	// snapshot. Only destination-local metadata is changed in this private file.
	for _, statement := range []string{
		"DELETE FROM meta WHERE key NOT IN ('version','cursor','paused','last_checkpoint_at','revision','search_generation')",
		"DELETE FROM checkpoint_receipts",
		"DELETE FROM root_turns",
		"UPDATE sources SET local_origin_ordinal=NULL WHERE EXISTS(SELECT 1 FROM evidence_units WHERE source_id=sources.id AND review_state='pending')",
		"UPDATE working_state SET local_origin_ordinal=0",
	} {
		if _, err = tx.ExecContext(l.ctx, statement); err != nil {
			return "", status, cleanup, err
		}
	}
	epoch := make([]byte, 16)
	if _, err = rand.Read(epoch); err != nil {
		return "", status, cleanup, err
	}
	for key, value := range map[string]string{"session": destination, "forked_from": l.session, "forked_from_store": l.store, "root_completed_ordinal": "0", "pagination_epoch": fmt.Sprintf("%x", epoch)} {
		if err = setMeta(l.ctx, tx, key, value); err != nil {
			return "", status, cleanup, err
		}
	}
	handoffBoundary(l.ctx, "metadata")
	if err = l.ctx.Err(); err != nil {
		return "", status, cleanup, err
	}

	if err = tx.Commit(); err != nil {
		return "", status, cleanup, err
	}
	target := &Ledger{db: copied, ctx: l.ctx, Path: path, session: destination, store: l.store}
	status, err = target.Status()
	return path, status, cleanup, err
}

func (l *Ledger) Fork(destination string) (Status, error) {
	if _, err := cleanText(destination, 256); err != nil {
		return Status{}, err
	}
	if destination == l.session {
		return Status{}, fmt.Errorf("choose a different destination session")
	}
	name, err := identity("session-", destination)
	if err != nil {
		return Status{}, err
	}
	path, status, cleanup, err := l.snapshot(destination)
	if err != nil {
		return Status{}, err
	}
	defer cleanup()
	if err = l.ctx.Err(); err != nil {
		return Status{}, err
	}
	dir := filepath.Join(l.store, name)
	if err = os.Mkdir(dir, 0700); err != nil {
		return Status{}, fmt.Errorf("destination already exists or cannot be created: %w", err)
	}
	handoffBoundary(l.ctx, "publication")
	if err = l.ctx.Err(); err != nil {
		_ = os.Remove(dir) // Never remove a competing startup's nonempty directory.
		return Status{}, err
	}
	final := filepath.Join(dir, "memory.sqlite3")
	// A hard link publishes the complete file without replacing a competing
	// startup's database. Both paths are in the same store/filesystem.
	if err = os.Link(path, final); err != nil {
		_ = os.Remove(dir) // Only removes an empty directory; never another writer's data.
		return Status{}, err
	}
	status.Database = final
	return status, nil
}

// Import seeds a session that has no prepared memory. Pending destination
// sources (including its initial prompt) remain pending after the source's
// snapshot. The destination transaction serializes competing hooks/imports.
func (l *Ledger) Import(sourceStore, sourceSession string) (Status, error) {
	if sourceStore == "" {
		return Status{}, fmt.Errorf("--from-store is required")
	}
	if _, err := cleanText(sourceSession, 256); err != nil {
		return Status{}, err
	}
	name, err := identity("session-", sourceSession)
	if err != nil {
		return Status{}, err
	}
	// A typo must not create a new source ledger and pretend an empty import
	// succeeded. Open subsequently verifies its stored identity and schema.
	if _, err = os.Stat(filepath.Join(sourceStore, name, "memory.sqlite3")); err != nil {
		return Status{}, fmt.Errorf("source session does not exist: %w", err)
	}
	source, err := Open(l.ctx, sourceStore, sourceSession)
	if err != nil {
		return Status{}, err
	}
	defer source.Close()
	if source.Path == l.Path {
		return Status{}, fmt.Errorf("choose a different source session")
	}
	path, _, cleanup, err := source.snapshot(l.session)
	if err != nil {
		return Status{}, err
	}
	defer cleanup()
	conn, err := l.db.Conn(l.ctx)
	if err != nil {
		return Status{}, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(l.ctx, "ATTACH DATABASE ? AS incoming", fileURI(path)); err != nil {
		return Status{}, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(ctx, "DETACH DATABASE incoming")
	}()
	tx, err := conn.BeginTx(l.ctx, nil)
	if err != nil {
		return Status{}, err
	}
	defer tx.Rollback()
	var prepared bool
	if err = tx.QueryRowContext(l.ctx, `SELECT
  EXISTS(SELECT 1 FROM main.entries) OR
  EXISTS(SELECT 1 FROM main.evidence_units WHERE review_state!='pending') OR
  EXISTS(SELECT 1 FROM main.working_state) OR
  EXISTS(SELECT 1 FROM main.meta WHERE key='forked_from' AND value!='') OR
  EXISTS(SELECT 1 FROM main.meta WHERE key='cursor' AND value!='0')`).Scan(&prepared); err != nil {
		return Status{}, err
	}
	if prepared {
		return Status{}, fmt.Errorf("destination already has checkpointed or imported memory; use a new session")
	}
	var revision, generation int64
	if err = tx.QueryRowContext(l.ctx, `SELECT
  CAST((SELECT value FROM main.meta WHERE key='revision') AS INTEGER),
  CAST((SELECT value FROM main.meta WHERE key='search_generation') AS INTEGER)`).Scan(&revision, &generation); err != nil {
		return Status{}, err
	}
	// Stage original order before replacing base rows. Existing FTS triggers
	// rebuild the indexes transactionally; copying shadow rows would duplicate them.
	// Conflicting IDs may only represent the same captured occurrence. A local
	// prompt from this session has a different origin identity from imported history.
	for _, statement := range []string{
		"CREATE TEMP TABLE pending_import AS SELECT seq,id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete FROM main.sources",
		"CREATE TEMP TABLE pending_units_import AS SELECT seq,id,source_id,start_byte,end_byte,review_state,deferral_reason FROM main.evidence_units",
		"DELETE FROM main.evidence_units",
		"DELETE FROM main.sources",
		"INSERT INTO main.sources(seq,id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete) SELECT seq,id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete FROM incoming.sources ORDER BY seq",
		"INSERT INTO main.evidence_units(seq,id,source_id,start_byte,end_byte,review_state,deferral_reason) SELECT seq,id,source_id,start_byte,end_byte,review_state,deferral_reason FROM incoming.evidence_units ORDER BY seq",
		"INSERT INTO main.sources(id,body,origin_session,origin_key,root_turn_id,local_origin_ordinal,origin_completion_ordinal,source_incomplete) SELECT id,body,origin_session,origin_key,root_turn_id,NULL,origin_completion_ordinal,source_incomplete FROM pending_import WHERE true ORDER BY seq ON CONFLICT(id) DO NOTHING",
		"INSERT INTO main.evidence_units(id,source_id,start_byte,end_byte,review_state,deferral_reason) SELECT id,source_id,start_byte,end_byte,'pending','' FROM pending_units_import WHERE true ORDER BY seq ON CONFLICT(id) DO NOTHING",
		"DROP TABLE pending_units_import",
		"DROP TABLE pending_import",
		"INSERT INTO main.entries(seq,id,body,active,retirement,effective_priority) SELECT seq,id,body,active,retirement,effective_priority FROM incoming.entries ORDER BY seq",
		"INSERT INTO main.working_state(singleton,body,revision,updated_at,local_origin_ordinal) SELECT singleton,body,revision,updated_at,0 FROM incoming.working_state",
		"DELETE FROM main.checkpoint_receipts",
		"DELETE FROM main.root_turns",
		"DELETE FROM main.meta WHERE key NOT IN ('version','session','paused','prompt_id')",
		"INSERT INTO main.meta(key,value) SELECT key,value FROM incoming.meta WHERE key IN ('cursor','last_checkpoint_at','forked_from','forked_from_store','root_completed_ordinal','pagination_epoch')",
	} {
		if _, err = tx.ExecContext(l.ctx, statement); err != nil {
			return Status{}, err
		}
	}
	for key, value := range map[string]string{"revision": strconv.FormatInt(revision+1, 10), "search_generation": strconv.FormatInt(generation+1, 10)} {
		if err = setMeta(l.ctx, tx, key, value); err != nil {
			return Status{}, err
		}
	}
	if _, err = tx.ExecContext(l.ctx, "UPDATE main.working_state SET revision=?", revision+1); err != nil {
		return Status{}, err
	}
	handoffBoundary(l.ctx, "import")
	if err = l.ctx.Err(); err != nil {
		return Status{}, err
	}

	if err = tx.Commit(); err != nil {
		return Status{}, err
	}
	handoffBoundary(l.ctx, "committed")
	// COMMIT is publication. Late caller cancellation must not report that the
	// import rolled back; bound final cleanup and status independently instead.
	committedCtx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 2*time.Second)
	defer cancel()
	// Release this sole connection before Status borrows one from the pool.
	if _, err = conn.ExecContext(committedCtx, "DETACH DATABASE incoming"); err != nil {
		return Status{}, err
	}
	if err = conn.Close(); err != nil {
		return Status{}, err
	}
	return (&Ledger{db: l.db, ctx: committedCtx, Path: l.Path, session: l.session, store: l.store}).Status()
}
