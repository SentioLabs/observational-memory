package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestObservationMetadataLockFailureRestoresNormalTimeout(t *testing.T) {
	l, err := Open(context.Background(), t.TempDir(), "observation-test")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.Pause(true); err != nil {
		t.Fatal(err)
	}
	if err = l.SetState("last_checkpoint_at", "2026-09-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range []int{2000, 321} {
		if _, err = l.db.Exec("PRAGMA busy_timeout=" + strconv.Itoa(timeout)); err != nil {
			t.Fatal(err)
		}
		paused, at, err := l.ObservationMetadata()
		if err != nil || !paused || at != "2026-09-07T12:00:00Z" {
			t.Fatal(paused, at, err)
		}
		peer, err := sql.Open("sqlite", l.Path)
		if err != nil {
			t.Fatal(err)
		}
		peer.SetMaxOpenConns(1)
		if _, err = peer.Exec("BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		paused, at, err = l.ObservationMetadata()
		elapsed := time.Since(start)
		if err == nil || !paused || at != "" || elapsed > 500*time.Millisecond {
			t.Fatal("metadata must fail closed promptly", paused, at, err, elapsed)
		}
		var actual int
		if err = l.db.QueryRow("PRAGMA busy_timeout").Scan(&actual); err != nil || actual != timeout {
			t.Fatal("normal timeout changed", actual, err)
		}
		peer.Exec("ROLLBACK")
		peer.Close()
		if paused, err = l.Paused(); err != nil || !paused {
			t.Fatal("normal privacy changed", paused, err)
		}
	}
}
func TestObservationMetadataCancelledContextPreservesConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l, err := Open(ctx, t.TempDir(), "cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cancel()
	paused, _, err := l.ObservationMetadata()
	if err == nil || !paused {
		t.Fatal("cancelled metadata accepted")
	}
	var timeout int
	if err = l.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 2000 {
		t.Fatal(timeout, err)
	}
}

func TestExistingObservationMetadataIdentityPrivacyAndReadOnly(t *testing.T) {
	store := t.TempDir()
	l, err := Open(context.Background(), store, "existing")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.SetState("last_checkpoint_at", "2026-09-07T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for _, wantPaused := range []bool{false, true} {
		if err = l.Pause(wantPaused); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(l.Path)
		if err != nil {
			t.Fatal(err)
		}
		paused, at, err := ExistingObservationMetadata(context.Background(), store, "existing")
		if err != nil || paused != wantPaused || at != "2026-09-07T12:00:00Z" {
			t.Fatal(paused, at, err)
		}
		after, err := os.ReadFile(l.Path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("metadata read changed memory", err)
		}
	}
	for _, entry := range []struct{ key, value string }{{"version", "1"}, {"session", "wrong"}} {
		t.Run(entry.key, func(t *testing.T) {
			var original string
			if err := l.db.QueryRow("SELECT value FROM meta WHERE key=?", entry.key).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if _, err := l.db.Exec("UPDATE meta SET value=? WHERE key=?", entry.value, entry.key); err != nil {
				t.Fatal(err)
			}
			paused, at, err := ExistingObservationMetadata(context.Background(), store, "existing")
			if err == nil || !paused || at != "" {
				t.Fatal("identity mismatch accepted", paused, at, err)
			}
			if _, err := l.db.Exec("UPDATE meta SET value=? WHERE key=?", original, entry.key); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExistingObservationMetadataRefusesUnsafeOrAbsentStore(t *testing.T) {
	for _, kind := range []string{"missing", "directory-permissions", "file-permissions", "session-symlink", "file-symlink", "sidecar-symlink", "wal"} {
		t.Run(kind, func(t *testing.T) {
			store := t.TempDir()
			if kind == "missing" {
				store = filepath.Join(store, "absent")
				paused, _, err := ExistingObservationMetadata(context.Background(), store, "test")
				if err == nil || !paused {
					t.Fatal("missing accepted")
				}
				if _, err := os.Lstat(store); !os.IsNotExist(err) {
					t.Fatal("created absent store", err)
				}
				return
			}
			l, err := Open(context.Background(), store, "test")
			if err != nil {
				t.Fatal(err)
			}
			path := l.Path
			if kind == "wal" {
				if _, err = l.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
					t.Fatal(err)
				}
			}
			if err = l.Close(); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory-permissions":
				err = os.Chmod(filepath.Dir(path), 0755)
			case "file-permissions":
				err = os.Chmod(path, 0644)
			case "session-symlink":
				dir := filepath.Dir(path)
				if err = os.Rename(dir, dir+"-original"); err == nil {
					err = os.Symlink(dir+"-original", dir)
				}
			case "file-symlink":
				if err = os.Rename(path, path+"-original"); err == nil {
					err = os.Symlink(path+"-original", path)
				}
			case "sidecar-symlink":
				err = os.Symlink(path, path+"-journal")
			}
			if err != nil {
				t.Fatal(err)
			}
			paused, at, err := ExistingObservationMetadata(context.Background(), store, "test")
			if err == nil || !paused || at != "" {
				t.Fatal("unsafe metadata accepted", paused, at, err)
			}
			if kind == "wal" {
				for _, suffix := range []string{"-wal", "-shm"} {
					if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
						t.Fatal("created WAL sidecar", suffix, err)
					}
				}
			}
		})
	}
}
