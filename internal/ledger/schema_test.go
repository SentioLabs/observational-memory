package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSchemaPreflightPreservesIncompatibleStore(t *testing.T) {
	for _, fixture := range []struct{ version, session string }{{"1", "test"}, {"3", "test"}, {"2", "wrong"}, {"", "test"}} {
		t.Run(fixture.version+fixture.session, func(t *testing.T) {
			store := t.TempDir()
			name, err := identity("session-", "test")
			check(t, err)
			dir := filepath.Join(store, name)
			check(t, os.Mkdir(dir, 0755))
			path := filepath.Join(dir, "memory.sqlite3")
			db, err := sql.Open("sqlite", path)
			check(t, err)
			_, err = db.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL); CREATE TABLE foreign_data(value TEXT); INSERT INTO foreign_data VALUES ('must survive')`)
			check(t, err)
			if fixture.version != "" {
				_, err = db.Exec(`INSERT INTO meta VALUES ('version',?),('session',?)`, fixture.version, fixture.session)
				check(t, err)
			}
			check(t, db.Close())
			check(t, os.Chmod(path, 0644))
			before, err := os.ReadFile(path)
			check(t, err)
			l, err := Open(context.Background(), store, "test")
			if err == nil {
				l.Close()
				t.Fatal("accepted incompatible store")
			}
			after, err := os.ReadFile(path)
			check(t, err)
			info, err := os.Stat(path)
			check(t, err)
			dirInfo, err := os.Stat(dir)
			check(t, err)
			if !bytes.Equal(before, after) || info.Mode().Perm() != 0644 || dirInfo.Mode().Perm() != 0755 {
				t.Fatal("preflight mutated incompatible store")
			}
			entries, err := os.ReadDir(dir)
			check(t, err)
			if len(entries) != 1 {
				t.Fatal("preflight left sidecar files")
			}
		})
	}
}

func TestSchemaPreflightConcurrentOpen(t *testing.T) {
	store := t.TempDir()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			l, err := Open(context.Background(), store, "same")
			if err == nil {
				_, err = l.Capture("user", "concurrent", "same")
				l.Close()
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		check(t, err)
	}
	l := openTest(t, store, "same")
	status, err := l.Status()
	check(t, err)
	if status.SourceCount != 1 || status.UnitCount != 1 {
		t.Fatal("concurrent initialization lost capture")
	}
	version, err := l.State("version")
	check(t, err)
	if version != "2" {
		t.Fatal("wrong schema")
	}
	info, err := os.Stat(l.Path)
	check(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("database permissions")
	}
}

func TestSchemaPreflightCanceledAndForeignEmpty(t *testing.T) {
	store := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l, err := Open(ctx, store, "canceled"); err == nil {
		l.Close()
		t.Fatal("opened canceled initialization")
	}
	name, err := identity("session-", "canceled")
	check(t, err)
	if _, err = os.Stat(filepath.Join(store, name)); !os.IsNotExist(err) {
		t.Fatal("canceled initialization reserved session")
	}
	name, err = identity("session-", "empty")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0700))
	path := filepath.Join(dir, "memory.sqlite3")
	check(t, os.WriteFile(path, nil, 0644))
	if l, err := Open(context.Background(), store, "empty"); err == nil {
		l.Close()
		t.Fatal("initialized foreign empty database")
	} else if !strings.Contains(err.Error(), "fresh") {
		t.Fatal("missing recovery instruction", err)
	}
	info, err := os.Stat(path)
	check(t, err)
	if info.Size() != 0 || info.Mode().Perm() != 0644 {
		t.Fatal("modified foreign empty database")
	}
}

func TestSchemaPreflightCanceledProvisioningCleansOnlyOwnedFiles(t *testing.T) {
	store := t.TempDir()
	path := filepath.Join(store, "memory.sqlite3")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l, err := createLedger(ctx, store, path, "new"); err == nil {
		l.Close()
		t.Fatal("created canceled ledger")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uncommitted owned file survived")
	}
	check(t, os.WriteFile(path+"-journal", []byte("foreign journal"), 0644))
	if l, err := createLedger(context.Background(), store, path, "new"); err == nil {
		l.Close()
		t.Fatal("accepted unowned SQLite journal")
	}
	journal, err := os.ReadFile(path + "-journal")
	check(t, err)
	if string(journal) != "foreign journal" {
		t.Fatal("modified unowned journal")
	}
	check(t, os.Remove(path+"-journal"))
	check(t, os.WriteFile(path, []byte("foreign"), 0644))
	if l, err := createLedger(ctx, store, path, "new"); err == nil {
		l.Close()
		t.Fatal("replaced foreign file")
	}
	data, err := os.ReadFile(path)
	check(t, err)
	if string(data) != "foreign" {
		t.Fatal("removed unowned file")
	}
}

func TestSchemaPreflightWaitsForCreatorAndCanCancel(t *testing.T) {
	store := t.TempDir()
	name, err := identity("session-", "waiting")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0700))
	marker := filepath.Join(dir, ".initializing")
	check(t, os.WriteFile(marker, nil, 0600))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		l, err := Open(ctx, store, "waiting")
		if l != nil {
			l.Close()
		}
		result <- err
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted cancellation, got %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("contender removed creator marker")
	}
	// Keep the marker visible until the whole transaction is committed.
	result = make(chan error, 1)
	go func() {
		l, err := Open(context.Background(), store, "waiting")
		if l != nil {
			l.Close()
		}
		result <- err
	}()
	l, err := createLedger(context.Background(), store, filepath.Join(dir, "memory.sqlite3"), "waiting")
	check(t, err)
	check(t, l.Close())
	check(t, os.Remove(marker))
	check(t, <-result)
}

func TestSchemaPreflightConstraintsAndCoreMetadata(t *testing.T) {
	l := openTest(t, t.TempDir(), "schema")
	for _, key := range []string{"cursor", "revision", "search_generation", "root_completed_ordinal"} {
		value, err := l.State(key)
		check(t, err)
		if value != "0" {
			t.Fatal("missing initial metadata", key, value)
		}
	}
	epoch, err := l.State("pagination_epoch")
	check(t, err)
	if epoch == "" {
		t.Fatal("missing pagination epoch")
	}
	other := openTest(t, l.store, "other")
	otherEpoch, err := other.State("pagination_epoch")
	check(t, err)
	if epoch == otherEpoch {
		t.Fatal("pagination epoch reused")
	}
	receipt, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: "unit"})
	check(t, err)
	for _, statement := range []string{
		`INSERT INTO evidence_units(id,source_id,start_byte,end_byte) VALUES('e-bad','missing',0,1)`,
		`INSERT INTO evidence_units(id,source_id,start_byte,end_byte) SELECT 'e-bad',id,1,1 FROM sources`,
		`UPDATE evidence_units SET review_state='understood'`,
		`INSERT INTO entries(id,body,effective_priority) VALUES('bad','{}',4)`,
		`INSERT INTO working_state VALUES(2,'{}',0,'',0)`,
		`DELETE FROM sources`,
	} {
		if _, err := l.db.Exec(statement); err == nil {
			t.Fatal("accepted violated schema constraint", statement)
		}
	}
	if len(readTestUnits(t, l, receipt.SourceID)) != 1 {
		t.Fatal("constraint failure damaged evidence")
	}
}

func TestSchemaPreflightWALRefusalDoesNotCreateSidecars(t *testing.T) {
	// A copied/crash-left WAL database may lack its shared-memory sidecar.
	// Refusing it must not create or alter files beside the original database.
	origin := filepath.Join(t.TempDir(), "origin.sqlite3")
	db, err := sql.Open("sqlite", origin)
	check(t, err)
	defer db.Close()
	_, err = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO meta VALUES('version','1'),('session','old')`)
	check(t, err)
	// A committed v2 schema living in the WAL must also be recognized.
	_, err = db.Exec(`UPDATE meta SET value='2' WHERE key='version'`)
	check(t, err)
	compatible := filepath.Join(t.TempDir(), "memory.sqlite3")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(origin + suffix)
		check(t, err)
		check(t, os.WriteFile(compatible+suffix, data, 0644))
	}
	check(t, preflightExisting(context.Background(), compatible, "old"))
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE); UPDATE meta SET value='1' WHERE key='version'`)
	check(t, err)
	store := t.TempDir()
	name, err := identity("session-", "old")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0755))
	path := filepath.Join(dir, "memory.sqlite3")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(origin + suffix)
		check(t, err)
		check(t, os.WriteFile(path+suffix, data, 0644))
	}
	before, err := os.ReadDir(dir)
	check(t, err)
	if l, err := Open(context.Background(), store, "old"); err == nil {
		l.Close()
		t.Fatal("accepted WAL v1")
	}
	after, err := os.ReadDir(dir)
	check(t, err)
	if len(before) != len(after) {
		t.Fatal("read-only preflight created a sidecar")
	}
	for _, suffix := range []string{"", "-wal"} {
		expected, err := os.ReadFile(origin + suffix)
		check(t, err)
		actual, err := os.ReadFile(path + suffix)
		check(t, err)
		if !bytes.Equal(expected, actual) {
			t.Fatal("preflight modified WAL database")
		}
	}
}
