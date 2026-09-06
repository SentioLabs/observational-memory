package ledger

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
	canonical, err := filepath.EvalSymlinks(dir)
	check(t, err)
	waitingPath := filepath.Join(canonical, "memory.sqlite3")
	deadline := time.Now().Add(time.Second)
	for {
		preflightReaders.Lock()
		retained := preflightReaders.paths[waitingPath] != nil
		preflightReaders.Unlock()
		if retained {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("waiting opener did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("wanted cancellation, got %v", err)
	}
	preflightReaders.Lock()
	retained := preflightReaders.paths[waitingPath] != nil
	preflightReaders.Unlock()
	if retained {
		t.Fatal("canceled opener retained reader lease")
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
	_, err = preflightExisting(context.Background(), compatible, "old")
	check(t, err)
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

func TestSchemaPreflightCrashWriter(t *testing.T) {
	path := os.Getenv("OM_TEST_CRASH_PATH")
	if path == "" {
		return
	}
	if os.Getenv("OM_TEST_CRASH_KIND") == "marker" {
		check(t, os.WriteFile(filepath.Join(filepath.Dir(path), ".initializing"), nil, 0600))
		_, err := createLedger(context.Background(), filepath.Dir(filepath.Dir(path)), path, "crash")
		check(t, err)
		os.Exit(0) // Commit succeeded, but the process never removes its marker.
	}
	db, err := sql.Open("sqlite", path)
	check(t, err)
	if os.Getenv("OM_TEST_CRASH_KIND") == "contender" {
		_, err = db.Exec(`PRAGMA busy_timeout=0; BEGIN IMMEDIATE`)
		if err == nil {
			os.Exit(23)
		}
		if !strings.Contains(err.Error(), "locked") {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	if os.Getenv("OM_TEST_CRASH_KIND") == "compatible" {
		_, err = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA cache_size=1; BEGIN IMMEDIATE;
UPDATE meta SET value='999' WHERE key='revision';
UPDATE evidence_units SET review_state='pending',deferral_reason='';
DELETE FROM checkpoint_receipts; DELETE FROM working_state; DELETE FROM entries;
UPDATE sources SET body=json_set(body,'$.text',replace(json_extract(body,'$.text'),'retained','unfinish'))`)
		check(t, err)
		fmt.Println("ready to kill")
		time.Sleep(time.Minute)
		t.Fatal("writer was not killed")
	}
	_, err = db.Exec(`PRAGMA cache_size=5; BEGIN IMMEDIATE; UPDATE meta SET value='2' WHERE key='version'; UPDATE meta SET value='crash' WHERE key='session'; UPDATE payload SET value=randomblob(4096)`)
	check(t, err)
	os.Exit(0) // Simulate a crash after dirty pages spill, without rollback/close.
}

func TestSchemaPreflightRecoversCompatibleHotJournal(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "compatible-crash")
	text := strings.Repeat("retained evidence line\n", 20000)
	log, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: text, Key: "log"})
	check(t, err)
	fact, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "Keep the committed constraint", Key: "fact"})
	check(t, err)
	cp := CheckpointV2{Acknowledge: []string{fact.FirstUnitID}, DeferSources: []SourceDeferral{{SourceID: log.SourceID, Reason: "Retained for exact recall"}},
		Observations: []ObservationV2{{Text: "Committed constraint", EvidenceIDs: []string{fact.FirstUnitID}}},
		WorkingState: &WorkingState{Objective: &WorkingFact{Text: "Preserve committed work", EvidenceIDs: []string{fact.FirstUnitID}}}}
	receipt, err := l.ApplyV2(cp)
	check(t, err)
	before := checkpointSnapshot(t, l)
	status, err := l.Status()
	check(t, err)
	path := l.Path
	check(t, l.Close())
	cmd := exec.Command(os.Args[0], "-test.run=^TestSchemaPreflightCrashWriter$")
	cmd.Env = append(os.Environ(), "OM_TEST_CRASH_PATH="+path, "OM_TEST_CRASH_KIND=compatible")
	stdout, err := cmd.StdoutPipe()
	check(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	check(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ready to kill"
	}()
	select {
	case ok := <-ready:
		if !ok {
			_ = cmd.Wait()
			t.Fatalf("writer failed before crash: %s", &stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not spill transaction")
	}
	check(t, cmd.Process.Kill())
	if err = cmd.Wait(); err == nil {
		t.Fatal("writer exited without being killed")
	}
	journal, err := os.ReadFile(path + "-journal")
	check(t, err)
	if len(journal) < 512 || bytes.Equal(journal[:8], make([]byte, 8)) {
		t.Fatal("fixture did not leave a hot journal")
	}
	l, err = Open(context.Background(), store, "compatible-crash")
	check(t, err)
	defer l.Close()
	if before != checkpointSnapshot(t, l) {
		t.Fatal("recovery changed committed checkpoint state")
	}
	gotStatus, err := l.Status()
	check(t, err)
	if !reflect.DeepEqual(gotStatus, status) {
		t.Fatal("recovery changed status", gotStatus, status)
	}
	gotSource, err := source(context.Background(), l.db, log.SourceID)
	check(t, err)
	if gotSource.Text != text {
		t.Fatal("recovery lost committed source text")
	}
	var reconstructed strings.Builder
	for _, unit := range readTestUnits(t, l, log.SourceID) {
		reconstructed.WriteString(unit.Text)
	}
	if reconstructed.String() != text {
		t.Fatal("recovered evidence no longer reconstructs source")
	}
	retry, err := l.ApplyV2(cp)
	check(t, err)
	want, err := EncodeResponse(receipt)
	check(t, err)
	got, err := EncodeResponse(retry)
	check(t, err)
	if !bytes.Equal(got, want) {
		t.Fatal("recovery changed retry receipt")
	}
	_, err = l.CaptureV2(CaptureInput{Kind: "assistant", Text: "Resumed after crash", Key: "resumed"})
	check(t, err)
}

func TestSchemaPreflightSnapshotGuardRejectsChangedOriginal(t *testing.T) {
	for _, mutation := range []string{"bytes", "replacement", "sidecar", "canceled"} {
		t.Run(mutation, func(t *testing.T) {
			store := t.TempDir()
			l := openTest(t, store, "crash")
			// A small independent payload forces journal spill without needing a
			// checkpoint fixture; public Open must refuse any stale validation.
			_, err := l.db.Exec(`CREATE TABLE payload(value BLOB); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<40) INSERT INTO payload SELECT zeroblob(4096) FROM n`)
			check(t, err)
			path := l.Path
			check(t, l.Close())
			runSchemaCrash(t, path, "journal")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			guard, err := preflightExisting(ctx, path, "crash")
			check(t, err)
			if guard == nil {
				t.Fatal("hot journal did not require guarded recovery")
			}
			data, err := os.ReadFile(path)
			check(t, err)
			switch mutation {
			case "bytes":
				info, err := os.Stat(path)
				check(t, err)
				data[len(data)-1] ^= 1
				check(t, os.WriteFile(path, data, 0600))
				// Restoring size/mtime does not defeat the content guard.
				check(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
			case "replacement":
				check(t, os.Rename(path, path+".previous"))
				check(t, os.WriteFile(path, data, 0600))
			case "sidecar":
				check(t, os.WriteFile(path+"-wal", []byte("concurrent sidecar"), 0600))
			case "canceled":
				cancel()
			}
			before := make(map[string][]byte)
			for _, suffix := range []string{"", "-journal"} {
				before[suffix], err = os.ReadFile(path + suffix)
				check(t, err)
			}
			if opened, err := openExisting(ctx, store, path, filepath.Dir(path), "crash", guard); err == nil {
				opened.Close()
				t.Fatal("recovered a changed/canceled original")
			}
			for suffix, want := range before {
				got, err := os.ReadFile(path + suffix)
				check(t, err)
				if !bytes.Equal(got, want) {
					t.Fatal("failed guard changed original", suffix)
				}
			}
		})
	}
}

func TestSchemaPreflightHealthyOpenNeedsNoTemporaryCopy(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "healthy")
	check(t, l.Close())
	t.Setenv("TMPDIR", filepath.Join(store, "unavailable-temp"))
	l, err := Open(context.Background(), store, "healthy")
	check(t, err)
	check(t, l.Close())
}

func TestSchemaPreflightRefusesExternalSuperJournal(t *testing.T) {
	l := openTest(t, t.TempDir(), "crash")
	_, err := l.db.Exec(`CREATE TABLE payload(value BLOB); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<40) INSERT INTO payload SELECT zeroblob(4096) FROM n`)
	check(t, err)
	check(t, l.Close())
	runSchemaCrash(t, l.Path, "journal")
	external := filepath.Join(t.TempDir(), "super-journal")
	check(t, os.WriteFile(external, []byte(l.Path+"-journal\x00"), 0644))
	journal, err := os.ReadFile(l.Path + "-journal")
	check(t, err)
	var footer [16]byte
	binary.BigEndian.PutUint32(footer[:4], uint32(len(external)))
	var checksum uint32
	for _, b := range []byte(external) {
		checksum += uint32(b)
	}
	binary.BigEndian.PutUint32(footer[4:8], checksum)
	copy(footer[8:], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7})
	journal = append(journal, []byte(external)...)
	journal = append(journal, footer[:]...)
	check(t, os.WriteFile(l.Path+"-journal", journal, 0600))
	before := make(map[string][]byte)
	for _, path := range []string{l.Path, l.Path + "-journal", external} {
		before[path], err = os.ReadFile(path)
		check(t, err)
	}
	opened, err := Open(context.Background(), l.store, "crash")
	if opened != nil {
		opened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "super-journal") {
		t.Fatal("did not refuse external journal reference", err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		check(t, err)
		if !bytes.Equal(got, want) {
			t.Fatal("preflight modified journal family", path)
		}
	}
}

func runSchemaCrash(t *testing.T, path, kind string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSchemaPreflightCrashWriter$")
	cmd.Env = append(os.Environ(), "OM_TEST_CRASH_PATH="+path, "OM_TEST_CRASH_KIND="+kind)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash fixture: %v\n%s", err, out)
	}
}

func TestSchemaPreflightReopensCommittedAbandonedMarker(t *testing.T) {
	store := t.TempDir()
	name, err := identity("session-", "crash")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0700))
	path := filepath.Join(dir, "memory.sqlite3")
	runSchemaCrash(t, path, "marker")
	l, err := Open(context.Background(), store, "crash")
	check(t, err)
	defer l.Close()
	_, err = l.Capture("user", "after interrupted startup", "recovered")
	check(t, err)
	if _, err = os.Stat(filepath.Join(dir, ".initializing")); err != nil {
		t.Fatal("opener removed another creator's marker")
	}
}

func TestSchemaPreflightRefusesHotJournalWithoutRecovery(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run("committed-version-"+version, func(t *testing.T) {
			committedSession := "crash"
			if version == "2" {
				committedSession = "wrong-session"
			}
			store := t.TempDir()
			name, err := identity("session-", "crash")
			check(t, err)
			dir := filepath.Join(store, name)
			check(t, os.Mkdir(dir, 0755))
			path := filepath.Join(dir, "memory.sqlite3")
			db, err := sql.Open("sqlite", path)
			check(t, err)
			_, err = db.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO meta VALUES('version',?),('session',?); CREATE TABLE payload(value BLOB); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<400) INSERT INTO payload SELECT zeroblob(4096) FROM n;`, version, committedSession)
			check(t, err)
			check(t, db.Close())
			runSchemaCrash(t, path, "journal")
			probe, err := sql.Open("sqlite", fileURI(path)+"?mode=ro&immutable=1")
			check(t, err)
			spilledVersion, err := meta(context.Background(), probe, "version")
			check(t, err)
			spilledSession, err := meta(context.Background(), probe, "session")
			check(t, err)
			check(t, probe.Close())
			if spilledVersion != "2" || spilledSession != "crash" {
				t.Fatalf("fixture did not spill uncommitted metadata: %q %q", spilledVersion, spilledSession)
			}
			before := map[string][]byte{}
			for _, suffix := range []string{"", "-journal"} {
				check(t, os.Chmod(path+suffix, 0644))
				before[suffix], err = os.ReadFile(path + suffix)
				check(t, err)
			}
			if len(before["-journal"]) < 512 || bytes.Equal(before["-journal"][:8], make([]byte, 8)) {
				t.Fatal("fixture is not a hot journal")
			}
			if l, err := Open(context.Background(), store, "crash"); err == nil {
				l.Close()
				t.Fatal("accepted incompatible hot journal")
			}
			for suffix, want := range before {
				got, err := os.ReadFile(path + suffix)
				check(t, err)
				info, err := os.Stat(path + suffix)
				check(t, err)
				if !bytes.Equal(got, want) || info.Mode().Perm() != 0644 {
					t.Fatal("refusal recovered or changed original", suffix)
				}
			}
			info, err := os.Stat(dir)
			check(t, err)
			if info.Mode().Perm() != 0755 {
				t.Fatal("refusal changed directory mode")
			}
			entries, err := os.ReadDir(dir)
			check(t, err)
			if len(entries) != 2 {
				t.Fatal("refusal changed sidecar inventory")
			}
		})
	}
}

func TestSchemaPreflightTightensExistingEmptyDirectory(t *testing.T) {
	store := t.TempDir()
	name, err := identity("session-", "existing-dir")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0755))
	l := openTest(t, store, "existing-dir")
	info, err := os.Stat(dir)
	check(t, err)
	if info.Mode().Perm() != 0700 {
		t.Fatal("successful new ledger left directory accessible")
	}
	info, err = os.Stat(l.Path)
	check(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("database permissions")
	}
}

func TestSchemaPreflightWALModeWithoutSidecars(t *testing.T) {
	store := t.TempDir()
	name, err := identity("session-", "wal-closed")
	check(t, err)
	dir := filepath.Join(store, name)
	check(t, os.Mkdir(dir, 0755))
	path := filepath.Join(dir, "memory.sqlite3")
	db, err := sql.Open("sqlite", path)
	check(t, err)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL); INSERT INTO meta VALUES('version','1'),('session','wal-closed')`)
	check(t, err)
	check(t, db.Close())
	check(t, os.Chmod(path, 0644))
	before, err := os.ReadFile(path)
	check(t, err)
	entries, err := os.ReadDir(dir)
	check(t, err)
	if len(entries) != 1 {
		t.Fatal("fixture did not checkpoint/remove WAL")
	}
	if l, err := Open(context.Background(), store, "wal-closed"); err == nil {
		l.Close()
		t.Fatal("accepted incompatible WAL mode")
	}
	after, err := os.ReadFile(path)
	check(t, err)
	entries, err = os.ReadDir(dir)
	check(t, err)
	if !bytes.Equal(before, after) || len(entries) != 1 {
		t.Fatal("read-only refusal changed checkpointed WAL store")
	}
}

func TestSchemaPreflightDuringActiveRollbackWriter(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "active-writer")
	tx, err := l.db.BeginTx(context.Background(), nil)
	check(t, err)
	defer tx.Rollback()
	check(t, setMeta(context.Background(), tx, "inflight", "not committed"))
	opened, err := Open(context.Background(), store, "active-writer")
	check(t, err)
	defer opened.Close()
	value, err := opened.State("inflight")
	check(t, err)
	if value != "" {
		t.Fatal("preflight/open exposed uncommitted state")
	}
	// Another process must still be excluded: closing a raw database descriptor
	// in this process would silently release the existing SQLite writer's locks.
	runSchemaCrash(t, l.Path, "contender")
	check(t, tx.Commit())
	value, err = opened.State("inflight")
	check(t, err)
	if value != "not committed" {
		t.Fatal("reader did not see later committed state")
	}
}

func TestSchemaPreflightAliasesKeepWriterLocked(t *testing.T) {
	for _, alias := range []string{"hardlink", "symlink"} {
		t.Run(alias, func(t *testing.T) {
			l := openTest(t, t.TempDir(), "alias")
			store := t.TempDir()
			name, err := identity("session-", "alias")
			check(t, err)
			dir := filepath.Join(store, name)
			if alias == "hardlink" {
				check(t, os.Mkdir(dir, 0700))
				check(t, os.Link(l.Path, filepath.Join(dir, "memory.sqlite3")))
			} else {
				check(t, os.Symlink(filepath.Dir(l.Path), dir))
			}
			tx, err := l.db.BeginTx(context.Background(), nil)
			check(t, err)
			defer tx.Rollback()
			check(t, setMeta(context.Background(), tx, "inflight", "alias lock"))
			other, err := Open(context.Background(), store, "alias")
			check(t, err)
			check(t, other.Close())
			runSchemaCrash(t, l.Path, "contender")
			check(t, tx.Commit())
		})
	}
}

func TestSchemaPreflightReaderLifetimeAndReuse(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "readers")
	for range 30 {
		other, err := Open(context.Background(), store, "readers")
		check(t, err)
		check(t, other.Close())
		check(t, other.Close())
	}
	info, err := os.Stat(l.Path)
	check(t, err)
	var readers []*os.File
	preflightReaders.Lock()
	for _, pinned := range preflightReaders.files {
		if pinned.info != nil && os.SameFile(info, pinned.info) {
			readers = append(readers, pinned.file)
		}
	}
	preflightReaders.Unlock()
	if len(readers) != 1 {
		t.Fatalf("repeated opens retained %d raw descriptors", len(readers))
	}
	check(t, l.Close())
	for _, f := range readers {
		if _, err = f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("last close left raw descriptor open", err)
		}
	}
	preflightReaders.Lock()
	remaining := preflightReaders.paths[l.Path] != nil
	preflightReaders.Unlock()
	if remaining {
		t.Fatal("last close retained path lease")
	}
	// A refusal also drops the lease without touching the incompatible file.
	db, err := sql.Open("sqlite", l.Path)
	check(t, err)
	_, err = db.Exec(`UPDATE meta SET value='1' WHERE key='version'`)
	check(t, err)
	check(t, db.Close())
	if opened, err := Open(context.Background(), store, "readers"); err == nil {
		opened.Close()
		t.Fatal("accepted old schema")
	}
	preflightReaders.Lock()
	remaining = preflightReaders.paths[l.Path] != nil
	preflightReaders.Unlock()
	if remaining {
		t.Fatal("failed open retained path lease")
	}
}

func TestSchemaPreflightCloseWaitsForTransaction(t *testing.T) {
	l := openTest(t, t.TempDir(), "close-writer")
	other, err := Open(context.Background(), l.store, "close-writer")
	check(t, err)
	check(t, other.Close()) // Ensure preflight has a pinned descriptor.
	tx, err := l.db.BeginTx(context.Background(), nil)
	check(t, err)
	defer tx.Rollback()
	check(t, setMeta(context.Background(), tx, "inflight", "finish before close"))
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	deadline := time.Now().Add(time.Second)
	for l.db.Stats().WaitCount == 0 {
		select {
		case err := <-closed:
			t.Fatalf("Close returned with an active transaction: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not wait for transaction")
		}
		time.Sleep(time.Millisecond)
	}
	runSchemaCrash(t, l.Path, "contender")
	check(t, tx.Commit())
	check(t, <-closed)
	check(t, l.Close())
	opened, err := Open(context.Background(), l.store, "close-writer")
	check(t, err)
	defer opened.Close()
	value, err := opened.State("inflight")
	check(t, err)
	if value != "finish before close" {
		t.Fatal("concurrent Close lost committed transaction")
	}
}

type initializationEntropyBarrier struct {
	reader  io.Reader
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *initializationEntropyBarrier) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.reader.Read(p)
}

func TestSchemaPreflightInitializingAliasCancellationKeepsWriterLocked(t *testing.T) {
	if os.Getenv("OM_TEST_INITIALIZING_ALIAS") != "1" {
		// Keep the entropy override isolated from unrelated tests and restore it
		// only after the initializing goroutine has finished.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSchemaPreflightInitializingAliasCancellationKeepsWriterLocked$")
		cmd.Env = append(os.Environ(), "OM_TEST_INITIALIZING_ALIAS=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("initialization alias fixture: %v\n%s", err, out)
		}
		return
	}
	for _, cancelCreator := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-creator-%t", cancelCreator), func(t *testing.T) {
			store, err := filepath.EvalSymlinks(t.TempDir())
			check(t, err)
			aliasStore, err := filepath.EvalSymlinks(t.TempDir())
			check(t, err)
			name, err := identity("session-", "initializing-alias")
			check(t, err)
			dir := filepath.Join(store, name)
			check(t, os.Mkdir(dir, 0700))
			aliasDir := filepath.Join(aliasStore, name)
			check(t, os.Symlink(dir, aliasDir))
			path := filepath.Join(dir, "memory.sqlite3")
			aliasPath := filepath.Join(aliasDir, "memory.sqlite3")
			entered := make(chan struct{})
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			originalReader := rand.Reader
			rand.Reader = &initializationEntropyBarrier{reader: originalReader, entered: entered, release: release}
			creatorCtx, cancelCreation := context.WithCancel(context.Background())
			defer cancelCreation()
			type openResult struct {
				ledger *Ledger
				err    error
			}
			created := make(chan openResult, 1)
			go func() { l, err := Open(creatorCtx, store, "initializing-alias"); created <- openResult{l, err} }()
			joined := false
			t.Cleanup(func() {
				unblock()
				if !joined {
					result := <-created
					if result.ledger != nil {
						_ = result.ledger.Close()
					}
				}
				rand.Reader = originalReader
			})
			select {
			case <-entered:
			case result := <-created:
				joined = true
				if result.ledger != nil {
					_ = result.ledger.Close()
				}
				t.Fatalf("creator did not reach transaction barrier: %v", result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("creator did not enter initialization transaction")
			}
			aliasCtx, cancelAlias := context.WithCancel(context.Background())
			defer cancelAlias()
			aliasResult := make(chan openResult, 1)
			go func() { l, err := Open(aliasCtx, aliasStore, "initializing-alias"); aliasResult <- openResult{l, err} }()
			info, err := os.Stat(path)
			check(t, err)
			var pinned *os.File
			deadline := time.Now().Add(time.Second)
			for pinned == nil {
				preflightReaders.Lock()
				for _, candidate := range preflightReaders.files {
					if candidate.info != nil && os.SameFile(info, candidate.info) {
						pinned = candidate.file
					}
				}
				preflightReaders.Unlock()
				if time.Now().After(deadline) {
					t.Fatal("alias did not read unpublished database")
				}
				time.Sleep(time.Millisecond)
			}
			cancelAlias()
			alias := <-aliasResult
			if alias.ledger != nil {
				alias.ledger.Close()
			}
			if !errors.Is(alias.err, context.Canceled) {
				t.Fatalf("alias cancellation: %v", alias.err)
			}
			// Cancellation must not release the still-initializing creator's
			// process-wide SQLite lock through the alias's raw descriptor.
			runSchemaCrash(t, path, "contender")
			if cancelCreator {
				cancelCreation()
			}
			unblock()
			result := <-created
			joined = true
			if cancelCreator {
				if result.ledger != nil {
					result.ledger.Close()
					t.Fatal("canceled creator published database")
				}
				if !errors.Is(result.err, context.Canceled) {
					t.Fatalf("creator cancellation: %v", result.err)
				}
				if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed initialization left owned database", err)
				}
			} else {
				check(t, result.err)
				_, err = result.ledger.CaptureV2(CaptureInput{Kind: "user", Text: "Committed after alias canceled", Key: "committed"})
				check(t, err)
				check(t, result.ledger.Close())
			}
			if _, err = pinned.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("finished initializer left pinned descriptor", err)
			}
			preflightReaders.Lock()
			retained := preflightReaders.paths[path] != nil || preflightReaders.paths[aliasPath] != nil
			preflightReaders.Unlock()
			if retained {
				t.Fatal("finished initializer retained lease")
			}
			if _, err = os.Stat(filepath.Join(dir, ".initializing")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("finished initializer retained marker", err)
			}
		})
	}
}
