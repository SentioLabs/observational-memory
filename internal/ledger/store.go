package ledger

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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
	db        *sql.DB
	ctx       context.Context
	Path      string
	session   string
	store     string
	release   func()
	closeOnce sync.Once
	closeErr  error
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
	release := retainPreflightReaders(path)
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	publish := func(l *Ledger, err error) (*Ledger, error) {
		if l != nil {
			if info, statErr := os.Stat(path); statErr == nil {
				preflightReaders.Lock()
				rememberPreflightInode(preflightReaders.paths[path], info)
				preflightReaders.Unlock()
			} else {
				_ = l.Close()
				return nil, statErr
			}
			l.release = release
			retained = true
		}
		return l, err
	}
	marker := filepath.Join(dir, ".initializing")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if _, err = os.Stat(marker); err == nil {
			// A process can exit after committing but before removing its marker.
			// A successful committed-state probe lets us proceed without deleting any
			// state that could still belong to a live creator.
			if guard, probeErr := preflightExisting(ctx, path, session); probeErr == nil {
				return publish(openExisting(ctx, absolute, path, dir, session, guard))
			}
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
			guard, probeErr := preflightExisting(ctx, path, session)
			if probeErr != nil {
				// A creator may have published its marker between our two stat calls.
				if _, pending := os.Stat(marker); pending == nil {
					continue
				}
				return nil, probeErr
			}
			return publish(openExisting(ctx, absolute, path, dir, session, guard))
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
		if err == nil {
			if err = os.Chmod(dir, 0700); err != nil {
				_ = l.Close()
				l = nil
			}
		}
		_ = os.Remove(marker)
		return publish(l, err)
	}
}

// POSIX closes release all of this process's byte locks on an inode, including
// locks owned by another SQLite connection. Keep raw main-file readers pinned
// until every managed Open/Ledger for that inode (including aliases) has closed
// its SQLite handles.
// Reuse each inode's descriptor; SectionReader gives each caller its own offset.
var preflightReaders = struct {
	sync.Mutex
	paths map[string]*preflightReadersForPath
	files []preflightPinnedFile
}{paths: make(map[string]*preflightReadersForPath)}

type preflightReadersForPath struct {
	refs   int
	inodes []os.FileInfo
}

type preflightPinnedFile struct {
	file *os.File
	info os.FileInfo
}

func rememberPreflightInode(state *preflightReadersForPath, info os.FileInfo) {
	for _, known := range state.inodes {
		if os.SameFile(known, info) {
			return
		}
	}
	state.inodes = append(state.inodes, info)
}

func retainPreflightReaders(path string) func() {
	preflightReaders.Lock()
	state := preflightReaders.paths[path]
	if state == nil {
		state = &preflightReadersForPath{}
		preflightReaders.paths[path] = state
	}
	state.refs++
	if info, err := os.Stat(path); err == nil {
		rememberPreflightInode(state, info)
	}
	preflightReaders.Unlock()
	return func() {
		preflightReaders.Lock()
		defer preflightReaders.Unlock()
		state.refs--
		if state.refs == 0 {
			delete(preflightReaders.paths, path)
			retained := preflightReaders.files[:0]
			for _, pinned := range preflightReaders.files {
				active := false
				for _, other := range preflightReaders.paths {
					for _, info := range other.inodes {
						if pinned.info == nil || os.SameFile(pinned.info, info) {
							active = true
						}
					}
				}
				if active {
					retained = append(retained, pinned)
				} else {
					_ = pinned.file.Close()
				}
			}
			preflightReaders.files = retained
		}
	}
}

type preflightReadCloser struct {
	*io.SectionReader
}

func (r preflightReadCloser) Close() error { return nil }

func preflightReader(path string) (io.ReadCloser, error) {
	preflightReaders.Lock()
	defer preflightReaders.Unlock()
	state := preflightReaders.paths[path]
	if state == nil {
		// Private copies and unlocked sidecars.
		return os.Open(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cannot preflight nonregular SQLite file")
	}
	rememberPreflightInode(state, info)
	for _, pinned := range preflightReaders.files {
		if pinned.info != nil && os.SameFile(info, pinned.info) {
			return preflightReadCloser{io.NewSectionReader(pinned.file, 0, math.MaxInt64)}, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Retain even a replaced inode: closing it could unlock an older Ledger.
	opened, statErr := f.Stat()
	preflightReaders.files = append(preflightReaders.files, preflightPinnedFile{f, opened})
	if statErr != nil {
		return nil, statErr
	}
	rememberPreflightInode(state, opened)
	return preflightReadCloser{io.NewSectionReader(f, 0, math.MaxInt64)}, nil
}

func database(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	params := url.Values{"_pragma": {"busy_timeout(2000)", "foreign_keys(1)"}, "_txlock": {"immediate"}}
	if readOnly {
		params.Set("mode", "ro")
	} else {
		params.Set("mode", "rw")
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
func preflightExisting(ctx context.Context, path, session string) (func() error, error) {
	probe := func(path string, readOnly bool) error {
		db, err := database(ctx, path, readOnly)
		if err != nil {
			return err
		}
		defer db.Close()
		return validateStore(ctx, db, session)
	}
	// Healthy rollback databases need no copy. Read-only mode honors locks and
	// refuses hot-journal recovery; immutable mode would expose dirty pages.
	file, err := preflightReader(path)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 20)
	_, readErr := io.ReadFull(file, header)
	_ = file.Close()
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return nil, readErr
	}
	walMode := header[18] == 2 || header[19] == 2
	if _, err = os.Stat(path + "-wal"); err == nil {
		walMode = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !walMode {
		err = probe(path, true)
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_READONLY_ROLLBACK {
			return nil, err
		}
	}
	// Both hot-journal rollback and WAL shared-memory creation must stay private
	// until committed version/session metadata has authorized the original.
	dir, err := os.MkdirTemp("", "om-preflight-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	copied := filepath.Join(dir, "memory.sqlite3")
	// Capture identities before copying any file and compare both metadata and
	// bytes afterward. A writer/recovery racing this exceptional path must cause
	// a retry, never make a mixed main/journal snapshot authority for mutation.
	files := make(map[string]preflightFile)
	for _, suffix := range []string{"", "-wal", "-journal", "-shm"} {
		info, statErr := os.Stat(path + suffix)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		files[suffix] = preflightFile{info: info}
	}
	for suffix, state := range files {
		if state.info == nil || suffix == "-shm" {
			continue
		}
		if !state.info.Mode().IsRegular() {
			return nil, fmt.Errorf("cannot preflight nonregular SQLite file")
		}
		if err = copyPreflightFile(ctx, path+suffix, copied+suffix); err != nil {
			return nil, err
		}
		state.digest, err = preflightDigest(ctx, copied+suffix)
		if err != nil {
			return nil, err
		}
		files[suffix] = state
	}
	guard := func() error { return checkPreflightFiles(ctx, path, files) }
	if err = guard(); err != nil {
		return nil, err
	}
	// A super-journal names files outside the private directory. Refuse it rather
	// than letting private recovery consult or delete any original sibling file.
	if err = refuseSuperJournal(copied + "-journal"); err != nil {
		return nil, err
	}
	if err = probe(copied, false); err != nil {
		return nil, err
	}
	if err = guard(); err != nil {
		return nil, err
	}
	return guard, nil
}

type preflightFile struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

func preflightDigest(ctx context.Context, path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	f, err := preflightReader(path)
	if err != nil {
		return digest, err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err = ctx.Err(); err != nil {
			return digest, err
		}
		n, readErr := f.Read(buffer)
		_, _ = h.Write(buffer[:n])
		if readErr == io.EOF {
			copy(digest[:], h.Sum(nil))
			return digest, nil
		}
		if readErr != nil {
			return digest, readErr
		}
	}
}

func checkPreflightFiles(ctx context.Context, path string, files map[string]preflightFile) error {
	changed := fmt.Errorf("ledger changed during preflight; retry opening session")
	checkMetadata := func() error {
		for suffix, state := range files {
			info, err := os.Stat(path + suffix)
			if state.info == nil {
				if !errors.Is(err, os.ErrNotExist) {
					return changed
				}
				continue
			}
			if err != nil || !os.SameFile(info, state.info) || info.Size() != state.info.Size() || info.Mode() != state.info.Mode() || !info.ModTime().Equal(state.info.ModTime()) {
				return changed
			}
		}
		return ctx.Err()
	}
	if err := checkMetadata(); err != nil {
		return err
	}
	for suffix, state := range files {
		if state.info != nil && suffix != "-shm" {
			digest, err := preflightDigest(ctx, path+suffix)
			if err != nil {
				return err
			}
			if digest != state.digest {
				return changed
			}
		}
	}
	return checkMetadata()
}

func refuseSuperJournal(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 16 {
		return nil
	}
	var magic [8]byte
	if _, err = f.ReadAt(magic[:], info.Size()-8); err != nil {
		return err
	}
	if magic == [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7} {
		return fmt.Errorf("cannot safely preflight SQLite super-journal; recover with the originating application")
	}
	return nil
}

func copyPreflightFile(ctx context.Context, source, destination string) error {
	from, err := preflightReader(source)
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
func openExisting(ctx context.Context, store, path, dir, session string, guard func() error) (*Ledger, error) {
	if guard != nil {
		if err := guard(); err != nil {
			return nil, err
		}
	}
	db, err := database(ctx, path, false)
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
	// The lease was acquired before this inode existed. Register it before any
	// SQLite handle can lock it, so a canceled alias cannot close its raw reader
	// and release the creator's process-wide locks during initialization.
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, statErr
	}
	preflightReaders.Lock()
	if state := preflightReaders.paths[path]; state != nil {
		rememberPreflightInode(state, info)
	}
	preflightReaders.Unlock()
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := database(ctx, path, false)
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
func (l *Ledger) Close() error {
	l.closeOnce.Do(func() {
		// DB.Close alone can return with a transaction's connection still in use.
		// Own the single connection first, then close the pool and that connection
		// before releasing raw descriptors shared with other Ledger handles.
		conn, err := l.db.Conn(context.Background())
		l.closeErr = l.db.Close()
		if err == nil {
			l.closeErr = errors.Join(l.closeErr, conn.Close())
		}
		if l.release != nil {
			l.release()
		}
	})
	return l.closeErr
}
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
