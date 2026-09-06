package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
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
	if _, err = tx.ExecContext(l.ctx, "DELETE FROM meta WHERE key NOT IN ('version','cursor','paused','last_checkpoint_at')"); err != nil {
		return "", status, cleanup, err
	}
	for key, value := range map[string]string{"session": destination, "forked_from": l.session, "forked_from_store": l.store} {
		if err = setMeta(l.ctx, tx, key, value); err != nil {
			return "", status, cleanup, err
		}
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
	var entries int
	if err = tx.QueryRowContext(l.ctx, "SELECT COUNT(*) FROM main.entries").Scan(&entries); err != nil {
		return Status{}, err
	}
	through, err := cursor(l.ctx, tx)
	if err != nil {
		return Status{}, err
	}
	previous, err := meta(l.ctx, tx, "forked_from")
	if err != nil {
		return Status{}, err
	}
	if entries != 0 || through != 0 || previous != "" {
		return Status{}, fmt.Errorf("destination already has checkpointed or imported memory; use a new session")
	}
	for _, statement := range []string{
		"CREATE TEMP TABLE pending_import AS SELECT seq,id,body FROM main.sources",
		"DELETE FROM main.sources",
		"INSERT INTO main.sources(seq,id,body) SELECT seq,id,body FROM incoming.sources ORDER BY seq",
		"INSERT OR IGNORE INTO main.sources(id,body) SELECT id,body FROM pending_import ORDER BY seq",
		"DROP TABLE pending_import",
		"INSERT INTO main.entries SELECT * FROM incoming.entries",
		"INSERT OR REPLACE INTO main.meta SELECT key,value FROM incoming.meta WHERE key IN ('cursor','last_checkpoint_at','forked_from','forked_from_store')",
		"DELETE FROM main.meta WHERE key IN ('stop_turn','stop_prompt','notified_cursor')",
	} {
		if _, err = tx.ExecContext(l.ctx, statement); err != nil {
			return Status{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Status{}, err
	}
	// Release this sole connection before Status borrows one from the pool.
	if _, err = conn.ExecContext(l.ctx, "DETACH DATABASE incoming"); err != nil {
		return Status{}, err
	}
	if err = conn.Close(); err != nil {
		return Status{}, err
	}
	return l.Status()
}
