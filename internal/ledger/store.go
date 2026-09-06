package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE sources (seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT UNIQUE NOT NULL, body TEXT NOT NULL,
 origin_session TEXT NOT NULL, origin_key TEXT NOT NULL, root_turn_id TEXT NOT NULL DEFAULT '',
 local_origin_ordinal INTEGER, origin_completion_ordinal INTEGER, source_incomplete INTEGER NOT NULL DEFAULT 0);
CREATE TABLE evidence_units (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
 source_id TEXT NOT NULL REFERENCES sources(id), start_byte INTEGER NOT NULL,
 end_byte INTEGER NOT NULL CHECK(end_byte>start_byte),
 review_state TEXT NOT NULL DEFAULT 'pending' CHECK(review_state IN ('pending','reviewed','deferred')),
 deferral_reason TEXT NOT NULL DEFAULT '');
CREATE INDEX evidence_pending ON evidence_units(review_state,seq);
CREATE INDEX evidence_source ON evidence_units(source_id,seq);
CREATE TABLE entries (seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT UNIQUE NOT NULL, body TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1,
 retirement TEXT, effective_priority INTEGER NOT NULL CHECK(effective_priority BETWEEN 0 AND 3));
CREATE TABLE checkpoint_receipts (digest TEXT PRIMARY KEY, body BLOB NOT NULL);
CREATE TABLE working_state (singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 body TEXT NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL,
 local_origin_ordinal INTEGER NOT NULL);
CREATE TABLE root_turns (id TEXT PRIMARY KEY, completed_ordinal INTEGER UNIQUE,
 continuation_claimed INTEGER NOT NULL DEFAULT 0, continuation_prompt TEXT NOT NULL DEFAULT '');
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

// Open preflights existing files without changing their schema or permissions.
// An exclusive initialization marker distinguishes our unfinished creation from
// a foreign empty file. Contenders wait for publication and probe it again.
func Open(ctx context.Context, store, session string) (*Ledger, error) {
	if _, err := cleanText(session, 256); err != nil {
		return nil, err
	}
	if store == "" {
		return nil, fmt.Errorf("store is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	createdDir := false
	if err = os.Mkdir(dir, 0700); err == nil {
		createdDir = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	defer func() {
		if createdDir {
			_ = os.Remove(dir)
		}
	}() // Empty directories only.
	path := filepath.Join(dir, "memory.sqlite3")
	marker := filepath.Join(dir, ".initializing")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if _, err = os.Stat(marker); err == nil {
			if !time.Now().Before(deadline) {
				return nil, fmt.Errorf("ledger initialization still in progress; retry opening session")
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if _, err = os.Stat(path); err == nil {
			if err = preflightExisting(ctx, path, session); err != nil {
				// A creator may have published its marker between our two stat calls.
				if _, pending := os.Stat(marker); pending == nil {
					continue
				}
				return nil, err
			}
			return openExisting(ctx, absolute, path, dir, session)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		lock, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_ = lock.Close()
		// Recheck after claiming initialization; another opener may have finished.
		if _, err = os.Stat(path); err == nil {
			_ = os.Remove(marker)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(marker)
			return nil, err
		}
		l, err := createLedger(ctx, absolute, path, session)
		_ = os.Remove(marker)
		return l, err
	}
}

func database(ctx context.Context, path string, readOnly, immutable bool) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	params := url.Values{"_pragma": {"busy_timeout(2000)", "foreign_keys(1)"}, "_txlock": {"immediate"}}
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("mode", "rw")
	}
	if immutable {
		params.Set("immutable", "1")
	}
	uri.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func validateStore(ctx context.Context, q queryer, session string) error {
	version, err := meta(ctx, q, "version")
	if err != nil {
		return fmt.Errorf("unsupported ledger schema; choose a fresh store/session: %w", err)
	}
	if version != strconv.Itoa(LedgerSchemaV2) {
		return fmt.Errorf("unsupported ledger version %q; choose a fresh store/session", version)
	}
	stored, err := meta(ctx, q, "session")
	if err != nil {
		return err
	}
	if stored != session {
		return fmt.Errorf("session identity mismatch; choose a fresh store/session")
	}
	return nil
}
func preflightExisting(ctx context.Context, path, session string) error {
	// Immutable mode probes the main database without touching a WAL's shared
	// memory. Identity/version never change after a store is provisioned.
	probe := func(path string, immutable bool) error {
		db, err := database(ctx, path, true, immutable)
		if err != nil {
			return err
		}
		defer db.Close()
		return validateStore(ctx, db, session)
	}
	err := probe(path, true)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	info, statErr := os.Stat(path + "-wal")
	if errors.Is(statErr, os.ErrNotExist) {
		return err
	}
	if statErr != nil {
		return statErr
	}
	if info.Size() == 0 {
		return err
	}
	// A fresh WAL database can hold its schema only in the log. SQLite's normal
	// read-only mode may CREATE -shm there, so probe private copies instead. The
	// originals are only opened for reading; all recovery side effects are private.
	dir, copyErr := os.MkdirTemp("", "om-preflight-*")
	if copyErr != nil {
		return copyErr
	}
	defer os.RemoveAll(dir)
	copied := filepath.Join(dir, "memory.sqlite3")
	for _, suffix := range []string{"", "-wal"} {
		if copyErr = copyPreflightFile(ctx, path+suffix, copied+suffix); copyErr != nil {
			// A concurrent checkpoint may have removed the WAL after the initial probe.
			if errors.Is(copyErr, os.ErrNotExist) {
				return probe(path, true)
			}
			return copyErr
		}
	}
	return probe(copied, false)
}

func copyPreflightFile(ctx context.Context, source, destination string) error {
	from, err := os.Open(source)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer to.Close()
	buffer := make([]byte, 64*1024)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := from.Read(buffer)
		if n > 0 {
			if _, err = to.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return to.Close()
		}
		if readErr != nil {
			return readErr
		}
	}
}
func openExisting(ctx context.Context, store, path, dir, session string) (*Ledger, error) {
	db, err := database(ctx, path, false, false)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Ledger, error) { _ = db.Close(); return nil, err }
	if err = validateStore(ctx, db, session); err != nil {
		return fail(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return fail(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err)
	}
	return &Ledger{db: db, ctx: ctx, Path: path, session: session, store: store}, nil
}
func createLedger(ctx context.Context, store, path, session string) (result *Ledger, err error) {
	// A missing main file does not grant ownership of preexisting SQLite logs.
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, err = os.Stat(path + suffix); err == nil {
			return nil, fmt.Errorf("existing SQLite sidecar; choose a fresh store/session")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			_ = os.Remove(path + "-journal")
			_ = os.Remove(path + "-wal")
			_ = os.Remove(path + "-shm")
			_ = os.Remove(path)
		}
	}()
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := database(ctx, path, false, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			_ = db.Close()
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	epoch := make([]byte, 16)
	if _, err = rand.Read(epoch); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{"session": session, "version": strconv.Itoa(LedgerSchemaV2), "cursor": "0", "revision": "0", "search_generation": "0", "root_completed_ordinal": "0", "pagination_epoch": fmt.Sprintf("%x", epoch)} {
		if err = setMeta(ctx, tx, key, value); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Ledger{db: db, ctx: ctx, Path: path, session: session, store: store}, nil
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
