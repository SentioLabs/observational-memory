package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sources (seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE NOT NULL, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entries (seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT UNIQUE NOT NULL, body TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1,
  retirement TEXT);
`

type Ledger struct {
	db      *sql.DB
	ctx     context.Context
	Path    string
	session string
	store   string
}
type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(ctx context.Context, store, session string) (*Ledger, error) {
	if _, err := cleanText(session, 256); err != nil {
		return nil, err
	}
	if store == "" {
		return nil, fmt.Errorf("store is required")
	}
	if err := os.MkdirAll(store, 0700); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(store)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	name, err := identity("session-", session)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(absolute, name)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "memory.sqlite3")
	// Set the mode before SQLite creates a journal alongside this file.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	params := url.Values{"_pragma": {"busy_timeout(2000)"}, "_txlock": {"immediate"}}
	uri.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	l := &Ledger{db: db, ctx: ctx, Path: path, session: session, store: absolute}
	fail := func(err error) (*Ledger, error) { _ = db.Close(); return nil, err }
	if _, err = db.ExecContext(ctx, schema); err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO meta VALUES ('session',?)", session); err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO meta VALUES ('version','1')"); err != nil {
		return fail(err)
	}
	stored, err := l.State("session")
	if err != nil {
		return fail(err)
	}
	if stored != session {
		return fail(fmt.Errorf("session identity mismatch"))
	}
	version, err := l.State("version")
	if err != nil {
		return fail(err)
	}
	if version != "1" {
		return fail(fmt.Errorf("unsupported ledger version %q", version))
	}
	return l, nil
}
func (l *Ledger) Close() error { return l.db.Close() }
func meta(ctx context.Context, q queryer, key string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}
func setMeta(ctx context.Context, q queryer, key, value string) error {
	_, err := q.ExecContext(ctx, "INSERT OR REPLACE INTO meta VALUES (?,?)", key, value)
	return err
}

// State stores transient adapter state. Fork excludes all non-core metadata.
func (l *Ledger) State(key string) (string, error) { return meta(l.ctx, l.db, key) }
func (l *Ledger) SetState(key, value string) error { return setMeta(l.ctx, l.db, key, value) }
func cursor(ctx context.Context, q queryer) (int64, error) {
	value, err := meta(ctx, q, "cursor")
	if err != nil || value == "" {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}
func (l *Ledger) Paused() (bool, error) { s, err := l.State("paused"); return s == "1", err }
func (l *Ledger) Pause(paused bool) error {
	value := "0"
	if paused {
		value = "1"
	}
	return l.SetState("paused", value)
}

type scanner interface{ Scan(...any) error }

func readSource(row scanner) (Source, error) {
	var seq int64
	var body string
	var source Source
	if err := row.Scan(&seq, &body); err != nil {
		return source, err
	}
	if err := json.Unmarshal([]byte(body), &source); err != nil {
		return source, err
	}
	source.Seq = seq
	return source, nil
}
func source(ctx context.Context, q queryer, id string) (Source, error) {
	return readSource(q.QueryRowContext(ctx, "SELECT seq,body FROM sources WHERE id=?", id))
}
func readEntry(row scanner) (Entry, error) {
	var seq int64
	var body string
	var active bool
	var retired sql.NullString
	var entry Entry
	if err := row.Scan(&seq, &body, &active, &retired); err != nil {
		return entry, err
	}
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		return entry, err
	}
	entry.Seq = seq
	entry.Active = active
	entry.Retirement = nil
	if retired.Valid {
		if err := json.Unmarshal([]byte(retired.String), &entry.Retirement); err != nil {
			return entry, err
		}
	}
	return entry, nil
}
func entry(ctx context.Context, q queryer, id string) (Entry, error) {
	return readEntry(q.QueryRowContext(ctx, "SELECT seq,body,active,retirement FROM entries WHERE id=?", id))
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
	dir := filepath.Join(l.store, name)
	if err = os.Mkdir(dir, 0700); err != nil {
		return Status{}, fmt.Errorf("destination already exists or cannot be created: %w", err)
	}
	path := filepath.Join(dir, "memory.sqlite3")
	conn, err := l.db.Conn(l.ctx)
	if err != nil {
		return Status{}, err
	}
	err = conn.Raw(func(raw any) error {
		api, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite backup API unavailable")
		}
		backup, err := api.NewBackup((&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String())
		if err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			if err = l.ctx.Err(); err != nil {
				break
			}
			more, stepErr := backup.Step(100)
			if stepErr != nil {
				err = stepErr
				break
			}
			if !more {
				break
			}
			if time.Now().After(deadline) {
				err = fmt.Errorf("snapshot exceeded two-second budget")
				break
			}
		}
		return errors.Join(err, backup.Finish())
	})
	closeErr := conn.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return Status{}, err
	}
	// The copied session marker must be rewritten before Open validates it.
	copied, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String())
	if err != nil {
		return Status{}, err
	}
	_, err = copied.ExecContext(l.ctx, "UPDATE meta SET value=? WHERE key='session'", destination)
	closeErr = copied.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return Status{}, err
	}
	target, err := Open(l.ctx, l.store, destination)
	if err != nil {
		return Status{}, err
	}
	defer target.Close()
	tx, err := target.db.BeginTx(l.ctx, nil)
	if err != nil {
		return Status{}, err
	}
	defer tx.Rollback()
	if err = setMeta(l.ctx, tx, "forked_from", l.session); err != nil {
		return Status{}, err
	}
	if _, err = tx.ExecContext(l.ctx, "DELETE FROM meta WHERE key NOT IN ('session','version','cursor','paused','forked_from')"); err != nil {
		return Status{}, err
	}
	if err = tx.Commit(); err != nil {
		return Status{}, err
	}
	return target.Status()
}
