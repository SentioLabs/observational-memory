package cli

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type telemetryLockedOutput struct {
	db       *sql.DB
	out      bytes.Buffer
	acquired bool
	at       time.Time
}

func (w *telemetryLockedOutput) Write(b []byte) (int, error) {
	if !w.acquired {
		if _, err := w.db.Exec("BEGIN EXCLUSIVE"); err != nil {
			return 0, err
		}
		w.acquired = true
		w.at = time.Now()
	}
	return w.out.Write(b)
}

// A peer obtains its legitimate write lock after prime output is complete.
// Supplemental telemetry must not wait on the normal ledger's two-second timeout.
func TestTelemetryMetadataDoesNotDelayCompletedOutput(t *testing.T) {
	for _, mode := range []string{"off", "disabled", "corrupt", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			store := t.TempDir()
			args := []string{"--store", store, "--session", "owned-lock-test", "prime"}
			baseline, err := run(t, args, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "off" {
				if _, err = run(t, []string{"--store", store, "telemetry", "enable"}, ""); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "disabled" {
				run(t, []string{"--store", store, "telemetry", "disable"}, "")
			}
			if mode == "corrupt" {
				os.WriteFile(filepath.Join(store, "telemetry", "events.sqlite3"), []byte("corrupt"), 0600)
			}
			paths, _ := filepath.Glob(filepath.Join(store, "session-*", "memory.sqlite3"))
			if len(paths) != 1 {
				t.Fatal(paths)
			}
			db, err := sql.Open("sqlite", paths[0])
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			w := &telemetryLockedOutput{db: db}
			cmd := New()
			cmd.SetArgs(args)
			cmd.SetOut(w)
			cmd.SetErr(io.Discard)
			err = cmd.Execute()
			elapsed := time.Since(w.at)
			db.Exec("ROLLBACK")
			if err != nil || !w.acquired || w.out.String() != baseline {
				t.Fatal("changed operation", err, w.acquired)
			}
			if elapsed > 500*time.Millisecond {
				t.Fatalf("telemetry delayed already-completed output for %v", elapsed)
			}
		})
	}
}

func TestTelemetryOnlyCompactHooksDoNotWaitForMemory(t *testing.T) {
	store := t.TempDir()
	run(t, []string{"--store", store, "--session", "compact-locked", "prime"}, "")
	run(t, []string{"--store", store, "telemetry", "enable"}, "")
	paths, _ := filepath.Glob(filepath.Join(store, "session-*", "memory.sqlite3"))
	db, err := sql.Open("sqlite", paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer db.Exec("ROLLBACK")
	for _, name := range []string{"PreCompact", "PostCompact"} {
		start := time.Now()
		out, err := run(t, []string{"--store", store, "hook", "--client", "codex"}, `{"hook_event_name":"`+name+`","session_id":"compact-locked","turn_id":"turn","trigger":"auto"}`)
		elapsed := time.Since(start)
		if err != nil || strings.TrimSpace(out) != "{}" || elapsed > 500*time.Millisecond {
			t.Errorf("optional %s failed open boundary: %s %v %v", name, out, err, elapsed)
		}
	}
}
