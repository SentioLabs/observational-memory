package ledger

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ObservationMetadata reads only privacy/checkpoint metadata for optional
// telemetry. It does not inherit the normal memory operation's lock wait.
// Unknown privacy state must cause the caller to suppress the observation.
func (l *Ledger) ObservationMetadata() (paused bool, checkpoint string, err error) {
	paused = true
	ctx, cancel := context.WithTimeout(l.ctx, 25*time.Millisecond)
	defer cancel()
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return true, "", err
	}
	defer conn.Close()
	var original int
	if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&original); err != nil {
		return true, "", err
	}
	defer func() {
		// The read may have used up or cancelled its context. Restoring connection
		// policy must have its own small allowance, never the expired context.
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer restoreCancel()
		if _, restoreErr := conn.ExecContext(restoreCtx, "PRAGMA busy_timeout="+strconv.Itoa(original)); restoreErr != nil {
			// A replacement connection receives the normal DSN policy. Never return a
			// connection with telemetry's zero wait to subsequent memory operations.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			paused, checkpoint, err = true, "", restoreErr
		}
	}()
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return true, "", err
	}
	var state string
	err = conn.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM meta WHERE key='paused'),''), COALESCE((SELECT value FROM meta WHERE key='last_checkpoint_at'),'')`).Scan(&state, &checkpoint)
	if err != nil {
		return true, "", err
	}
	return state != "" && state != "0", checkpoint, nil
}

// ExistingObservationMetadata is for telemetry-only hooks: never create a
// session, migrate/repair a file, or run ordinary ledger validation. Only a
// private existing file with matching version/session metadata can be sampled.
func ExistingObservationMetadata(parent context.Context, store, session string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Millisecond)
	defer cancel()
	if store == "" {
		return true, "", fmt.Errorf("store is required")
	}
	if _, err := cleanText(session, 256); err != nil {
		return true, "", err
	}
	name, err := identity("session-", session)
	if err != nil {
		return true, "", err
	}
	absolute, err := filepath.Abs(store)
	if err != nil {
		return true, "", err
	}
	dir := filepath.Join(absolute, name)
	path := filepath.Join(dir, "memory.sqlite3")
	check := func(path string, directory bool) error {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("private observation metadata unavailable")
		}
		return nil
	}
	if err = check(dir, true); err != nil {
		return true, "", err
	}
	if err = check(path, false); err != nil {
		return true, "", err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err = check(path+suffix, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, "", err
		}
	}
	// A read-only SQLite connection can create WAL shared-memory files. This
	// optional path declines WAL/recovery states instead of repairing or copying
	// them; normal ledger opening retains its existing recovery behavior.
	file, err := os.Open(path)
	if err != nil {
		return true, "", err
	}
	header := make([]byte, 20)
	_, err = io.ReadFull(file, header)
	_ = file.Close()
	if err != nil || string(header[:16]) != "SQLite format 3\x00" || header[18] != 1 || header[19] != 1 {
		return true, "", fmt.Errorf("observation metadata requires a rollback database")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(0)"}}.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return true, "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version, stored, state, checkpoint string
	err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM meta WHERE key='version'),''),COALESCE((SELECT value FROM meta WHERE key='session'),''),COALESCE((SELECT value FROM meta WHERE key='paused'),''),COALESCE((SELECT value FROM meta WHERE key='last_checkpoint_at'),'')`).Scan(&version, &stored, &state, &checkpoint)
	if err != nil {
		return true, "", err
	}
	if version != strconv.Itoa(LedgerSchemaV2) || stored != session {
		return true, "", fmt.Errorf("observation metadata identity unavailable")
	}
	return state != "" && state != "0", checkpoint, nil
}
