package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrivatePseudonymsLifecycleAndFailures(t *testing.T) {
	store := t.TempDir()
	if err := Configure(store, true); err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"PreCompact", "PreCompact", "PostCompact", "PostCompact", "prime"} {
		if err := Write(store, Input{Session: "PRIVATE-task", Turn: "PRIVATE-turn", Category: category, Model: "gpt-6-astra", Trigger: "auto", Version: "dev"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(store, Input{Session: "PRIVATE-task", Category: "search", Failed: true, Duration: 4 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	r, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if r["identified_compaction_turns_lower_bound"] != 1 || r["pre_post_turn_pairs_lower_bound"] != 1 || r["tasks_with_prime_after_compact_signal"] != 1 || r["unique_compactions"] != nil {
		t.Fatal(r)
	}
	data, _ := os.ReadFile(filepath.Join(store, "telemetry", "events.sqlite3"))
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("raw identity persisted")
	}
	for _, p := range []string{"telemetry", "telemetry/events.sqlite3"} {
		info, _ := os.Stat(filepath.Join(store, p))
		if info.Mode().Perm()&0077 != 0 {
			t.Fatal(info.Mode())
		}
	}
	db, err := open(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var task, body string
	db.QueryRow("SELECT task,body FROM events ORDER BY seq LIMIT 1").Scan(&task, &body)
	var e Event
	json.Unmarshal([]byte(body), &e)
	if len(task) != 32 || e.Task == e.Turn {
		t.Fatal(e)
	}
	other := t.TempDir()
	Configure(other, true)
	Write(other, Input{Session: "PRIVATE-task", Category: "prime"})
	db2, _ := open(context.Background(), other, false)
	defer db2.Close()
	var task2 string
	db2.QueryRow("SELECT task FROM events").Scan(&task2)
	if task == task2 {
		t.Fatal("cross-store correlation")
	}
}
func TestDisabledRetentionCorruptionAndLockBound(t *testing.T) {
	store := t.TempDir()
	Record(store, Input{Session: "s", Category: "prime"})
	if _, e := os.Stat(filepath.Join(store, "telemetry")); !os.IsNotExist(e) {
		t.Fatal("off wrote")
	}
	Configure(store, true)
	Write(store, Input{Session: "s", Category: "prime"})
	db, err := open(context.Background(), store, false)
	if err != nil {
		t.Fatal(err)
	}
	// Model a long retained sequence without paying for 100000 individual fsyncs.
	if _, err = db.Exec("UPDATE events SET seq=?", MaxEvents+1); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO events(seq,task,category,source,body) SELECT 1,task,category,source,body FROM events LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	Write(store, Input{Session: "s", Category: "status"})
	r, _ := Report(store)
	if r["retention_evicted_events"] != 1 || r["retention_cap_reached"] != true {
		t.Fatal(r)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE config SET enabled=enabled"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = Write(store, Input{Session: "s", Category: "prime"}); err == nil {
		t.Fatal("writer bypassed lock")
	}
	if time.Since(start) > time.Second {
		t.Fatal("lock not bounded")
	}
	tx.Rollback()
	db.Close()
	Configure(store, false)
	before, _ := os.ReadFile(filepath.Join(store, "telemetry", "events.sqlite3"))
	if err = Write(store, Input{Session: "s", Category: "prime"}); err != ErrDisabled {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(store, "telemetry", "events.sqlite3"))
	if string(before) != string(after) {
		t.Fatal("disabled modified DB")
	}
	Configure(store, true)
	r, _ = Report(store)
	if r["enrollment_epochs"] != 2 {
		t.Fatal(r)
	}
	path := filepath.Join(store, "telemetry", "events.sqlite3")
	os.WriteFile(path, []byte("corrupt PRIVATE"), 0600)
	start = time.Now()
	Record(store, Input{Category: "prime"})
	if time.Since(start) > time.Second {
		t.Fatal("corrupt blocked")
	}
	if _, err = Report(store); err == nil {
		t.Fatal("corrupt healthy")
	}
}
func TestFullUnwritableAndSymlinkRefusal(t *testing.T) {
	for _, kind := range []string{"full", "unwritable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			store := t.TempDir()
			Configure(store, true)
			path := filepath.Join(store, "telemetry", "events.sqlite3")
			switch kind {
			case "full":
				os.Truncate(path, maxBytes+1)
			case "unwritable":
				os.Chmod(path, 0400)
				defer os.Chmod(path, 0600)
			case "symlink":
				os.Remove(path)
				target := filepath.Join(t.TempDir(), "outside")
				os.WriteFile(target, []byte("PRIVATE"), 0600)
				os.Symlink(target, path)
			}
			start := time.Now()
			if err := Write(store, Input{Category: "prime"}); err == nil {
				t.Fatal("unsafe storage accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("unbounded")
			}
		})
	}
}
func TestTelemetryProcessHelper(t *testing.T) {
	if os.Getenv("OM_TELEMETRY_TEST_CHILD") == "" {
		return
	}
	if err := Write(os.Getenv("OM_TELEMETRY_TEST_STORE"), Input{Session: "task", Category: "prime"}); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}
func TestConcurrentProcesses(t *testing.T) {
	store := t.TempDir()
	if err := Configure(store, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := exec.Command(os.Args[0], "-test.run=^TestTelemetryProcessHelper$")
			c.Env = append(os.Environ(), "OM_TELEMETRY_TEST_CHILD=1", "OM_TELEMETRY_TEST_STORE="+store)
			if c.Run() == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	r, err := Report(store)
	if err != nil || success == 0 || r["observed_events"] != success {
		t.Fatal(success, r, err)
	}
}

func TestUnknownFieldsCoverageAndEnrollment(t *testing.T) {
	store := t.TempDir()
	Configure(store, true)
	r, _ := Report(store)
	if r["collection_state"] != "awaiting_first_hook" || r["first_observed_hook_at"] != nil {
		t.Fatal(r)
	}
	age := int64(12)
	c := SummaryCoverage(2, 30, 4, 60, 1, 20)
	Write(store, Input{Session: "s", Category: "status", Coverage: c, CheckpointAgeSeconds: &age, Model: "PRIVATE-MODEL"})
	Write(store, Input{Session: "s", Category: "PRIVATE-CATEGORY", Source: "PRIVATE-SOURCE", Trigger: "PRIVATE-TRIGGER"})
	Write(store, Input{Session: "s", Category: "PostCompact"})
	Write(store, Input{Session: "s", Category: "PostCompact"})
	r, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if r["identified_compaction_turns_lower_bound"] != 0 || r["pre_post_turn_pairs_lower_bound"] != 0 {
		t.Fatal("invented boundary", r)
	}
	if r["latest_observed_coverage_sum_not_current_store"].(Coverage).DeferredBytes != 20 || r["largest_checkpoint_age_seconds_at_last_observation"] != int64(12) {
		t.Fatal(r)
	}
	data, _ := os.ReadFile(filepath.Join(store, "telemetry", "events.sqlite3"))
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("unknown metadata persisted")
	}
}
