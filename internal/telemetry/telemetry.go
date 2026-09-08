// Package telemetry stores opt-in, content-free local operational observations.
package telemetry

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const MaxEvents = 100000
const maxBytes = 64 << 20

var ErrDisabled = errors.New("telemetry is disabled")

// Input has no content, path, arbitrary metadata or error-message field.
// Session and Turn are pseudonymized before serialization.
type Input struct {
	Session, Turn, Category, Source, Version, Feedback, Model, Trigger string
	Failed, Paused                                                     bool
	Duration                                                           time.Duration
	Coverage                                                           *Coverage
	CheckpointAgeSeconds                                               *int64
}
type Coverage struct{ PendingUnits, PendingBytes, ReviewedUnits, ReviewedBytes, DeferredUnits, DeferredBytes int64 }
type Event struct {
	Schema               int       `json:"schema"`
	At                   string    `json:"at"`
	Runtime              string    `json:"runtime"`
	Task                 string    `json:"task,omitempty"`
	Turn                 string    `json:"turn,omitempty"`
	Category             string    `json:"category"`
	Source               string    `json:"source,omitempty"`
	Model                *string   `json:"model"`
	Trigger              string    `json:"trigger,omitempty"`
	Outcome              string    `json:"outcome"`
	DurationMS           float64   `json:"duration_ms"`
	PrimeLoaded          bool      `json:"prime_loaded"`
	Coverage             *Coverage `json:"coverage"`
	CheckpointAgeSeconds *int64    `json:"checkpoint_age_seconds"`
	Feedback             string    `json:"feedback,omitempty"`
	CompactSignal        *int64    `json:"compact_signal_observation,omitempty"`
}

func category(s string) string {
	switch s {
	case "SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "Interrupt", "PreCompact", "PostCompact", "pending", "status", "prime", "capture", "apply", "recall", "search", "view", "fork", "import", "pause", "resume", "feedback":
		return s
	}
	return "other"
}
func source(s string) string {
	switch s {
	case "startup", "resume", "clear", "compact":
		return s
	}
	return "unknown"
}
func ValidFeedback(s string) bool {
	switch s {
	case "recovered", "forgot", "repeated-work", "correction":
		return true
	}
	return false
}
func pseudo(key []byte, domain, value string) string {
	if value == "" || len(value) > 1024 {
		return ""
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil)[:16])
}
func runtime(s string) string {
	if len(s) == 0 || len(s) > 64 {
		return "unknown"
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.ContainsRune(".+-_", c)) {
			return "unknown"
		}
	}
	return s
}
func private(path string, dir bool) error {
	s, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if s.Mode()&os.ModeSymlink != 0 || s.IsDir() != dir || (!dir && !s.Mode().IsRegular()) || s.Mode().Perm()&0077 != 0 {
		return errors.New("telemetry requires private regular storage")
	}
	return nil
}
func open(ctx context.Context, store string, create bool) (*sql.DB, error) {
	if store == "" {
		return nil, errors.New("telemetry requires an explicit store")
	}
	dir := filepath.Join(store, "telemetry")
	if create {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
	}
	if e := private(dir, true); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "events.sqlite3")
	if create {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e == nil {
			f.Close()
		} else if !errors.Is(e, os.ErrExist) {
			return nil, e
		}
	}
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if e := private(path+suffix, false); e != nil && !(suffix != "" && errors.Is(e, os.ErrNotExist)) {
			return nil, e
		}
	}
	info, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if info.Size() > maxBytes {
		return nil, errors.New("telemetry storage exceeds limit")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(25)", "max_page_count(16384)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func Configure(store string, enabled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db, err := open(ctx, store, enabled)
	if errors.Is(err, os.ErrNotExist) && !enabled {
		return nil
	}
	if err != nil {
		return errors.New("telemetry configuration unavailable")
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `PRAGMA max_page_count=16384; CREATE TABLE IF NOT EXISTS config (id INTEGER PRIMARY KEY CHECK(id=1), schema INTEGER, enabled INTEGER, key BLOB, enabled_at TEXT, first_hook TEXT, evicted INTEGER, epochs INTEGER); CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, task TEXT, category TEXT, source TEXT, body TEXT); CREATE INDEX IF NOT EXISTS task_events ON events(task,seq);`)
	if err != nil {
		return errors.New("telemetry configuration unavailable")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO config VALUES(1,1,?, ?,?,'',0,1) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, epochs=epochs+CASE WHEN config.enabled=0 AND excluded.enabled=1 THEN 1 ELSE 0 END WHERE config.schema=1`, enabled, key, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Enabled is a bounded read used to avoid adding memory work for telemetry-only hooks when off.
func Enabled(store string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	db, err := open(ctx, store, false)
	if err != nil {
		return false
	}
	defer db.Close()
	var enabled, version int
	return db.QueryRowContext(ctx, "SELECT enabled,schema FROM config WHERE id=1").Scan(&enabled, &version) == nil && enabled == 1 && version == 1
}

// Record never changes normal operation results. Unrecordable drops are unknown.
func Record(store string, input Input) { _ = Write(store, input) }
func Write(store string, input Input) error {
	if input.Paused {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	db, err := open(ctx, store, false)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled, version int
	var key []byte
	if err = tx.QueryRowContext(ctx, "SELECT enabled,schema,key FROM config WHERE id=1").Scan(&enabled, &version, &key); err != nil {
		return err
	}
	if enabled != 1 {
		return ErrDisabled
	}
	if version != 1 || len(key) != 32 {
		return errors.New("telemetry schema unavailable")
	}
	e := Event{Schema: 1, At: time.Now().UTC().Format(time.RFC3339Nano), Runtime: runtime(input.Version), Task: pseudo(key, "task", input.Session), Turn: pseudo(key, "turn:"+input.Session, input.Turn), Category: category(input.Category), Outcome: "ok", DurationMS: float64(input.Duration) / float64(time.Millisecond), Coverage: input.Coverage, CheckpointAgeSeconds: input.CheckpointAgeSeconds}
	if e.DurationMS < 0 {
		e.DurationMS = 0
	}
	if e.DurationMS > 86400000 {
		e.DurationMS = 86400000
	}
	if input.Failed {
		e.Outcome = "operation_failed"
	}
	e.PrimeLoaded = e.Category == "prime" && !input.Failed
	if e.Category == "SessionStart" {
		e.Source = source(input.Source)
	}
	if input.Model == "gpt-6-astra" || input.Model == "gpt-5.5" || input.Model == "gpt-5.4" || input.Model == "gpt-5.3-codex" {
		model := input.Model
		e.Model = &model
	}
	if e.Category == "PreCompact" || e.Category == "PostCompact" {
		e.Trigger = "unknown"
		if input.Trigger == "auto" || input.Trigger == "manual" {
			e.Trigger = input.Trigger
		}
	}
	if e.Category == "feedback" {
		if !ValidFeedback(input.Feedback) || e.Task == "" {
			return errors.New("feedback requires a session and closed rating")
		}
		e.Feedback = input.Feedback
	}
	if e.PrimeLoaded || e.Feedback != "" {
		var seq int64
		if tx.QueryRowContext(ctx, "SELECT seq FROM events WHERE task=? AND (category='PostCompact' OR (category='SessionStart' AND source='compact')) ORDER BY seq DESC LIMIT 1", e.Task).Scan(&seq) == nil {
			e.CompactSignal = &seq
		}
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(body) > 2048 {
		return errors.New("telemetry event exceeds limit")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO events(task,category,source,body) VALUES(?,?,?,?)", e.Task, e.Category, e.Source, string(body)); err != nil {
		return err
	}
	if e.Category == "SessionStart" || e.Category == "UserPromptSubmit" || e.Category == "PostToolUse" || e.Category == "Stop" || e.Category == "Interrupt" || e.Category == "PreCompact" || e.Category == "PostCompact" {
		if _, err = tx.ExecContext(ctx, "UPDATE config SET first_hook=? WHERE first_hook=''", e.At); err != nil {
			return err
		}
	}
	keep := MaxEvents
	var pages, freePages int
	if tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && tx.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages) == nil && pages-freePages > 14000 {
		var count int
		if tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&count) == nil {
			keep = count * 9 / 10
		}
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM events WHERE seq <= (SELECT MAX(seq)-? FROM events)", keep)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		if _, err = tx.ExecContext(ctx, "UPDATE config SET evicted=evicted+?", n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func Report(store string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := map[string]any{"schema": 1, "enabled": false, "enabled_at": nil, "first_observed_hook_at": nil, "collection_state": "disabled", "observed_events": 0, "observed_tasks": 0, "unique_compactions": nil, "model": nil, "host_version": nil, "provider_usage": nil, "host_activation_verified": false, "unrecorded_drops": nil, "retention_max_events": MaxEvents, "limitations": []string{"Local CLI observations do not attest real host activation or hooks that failed before CLI launch.", "Reference host 0.153.4 compact hooks identify turns, not compaction items; distinct turn pairs are lower bounds, repeated compactions per turn collapse. SessionStart lacks even turn identity. Raw signals may duplicate; exact unique compactions remain unknown.", "Prime success means loaded, not remembered. Unrated recovery is unknown; feedback is explicit user choice, not a causal comparison.", "Lock, disk, corruption, process-kill and privacy-pause gaps may be unrecorded; drops and unobserved tasks cannot be counted.", "Retained rows are bounded; per-command duration excludes process startup and telemetry persistence, not model time or billed tokens."}}
	db, err := open(ctx, store, false)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, errors.New("telemetry unavailable; normal memory remains independent")
	}
	defer db.Close()
	var enabled, version, evicted, epochs int
	var at, first string
	if err = db.QueryRowContext(ctx, "SELECT enabled,schema,enabled_at,first_hook,evicted,epochs FROM config WHERE id=1").Scan(&enabled, &version, &at, &first, &evicted, &epochs); err != nil || version != 1 {
		return nil, errors.New("telemetry unavailable; normal memory remains independent")
	}
	r["enabled"] = enabled == 1
	if enabled == 1 {
		r["collection_state"] = "awaiting_first_hook"
		if first != "" {
			r["collection_state"] = "hook_delivery_observed_host_unverified"
		}
	}
	r["enabled_at"] = at
	r["enrollment_epochs"] = epochs
	r["retention_evicted_events"] = evicted
	if first != "" {
		r["first_observed_hook_at"] = first
	}
	rows, err := db.QueryContext(ctx, "SELECT seq,body FROM events ORDER BY seq LIMIT ?", MaxEvents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks, loaded := map[string]bool{}, map[string]bool{}
	categories, errorsBy, feedback := map[string]int{}, map[string]int{}, map[string]int{}
	signals, coverage, linkedFeedback := 0, 0, 0
	latencies := map[string][]float64{}
	signalTasks := map[string]int{}
	pre, post := map[string]Event{}, map[string]Event{}
	models := map[string]int{}
	latestCoverage := map[string]Coverage{}
	checkpointAges := map[string]int64{}
	count := 0
	for rows.Next() {
		var seq int64
		var body string
		var e Event
		if err = rows.Scan(&seq, &body); err != nil {
			return nil, err
		}
		if len(body) > 2048 || json.Unmarshal([]byte(body), &e) != nil {
			return nil, errors.New("telemetry event unavailable")
		}
		count++
		if e.Model != nil {
			models[*e.Model]++
		}
		if e.Task != "" && e.Turn != "" {
			pair := e.Task + ":" + e.Turn
			if e.Category == "PreCompact" {
				if _, ok := pre[pair]; !ok {
					pre[pair] = e
				}
			}
			if e.Category == "PostCompact" {
				if _, ok := post[pair]; !ok {
					post[pair] = e
				}
			}
		}
		if e.Task != "" {
			tasks[e.Task] = true
		}
		categories[e.Category]++
		latencies[e.Category] = append(latencies[e.Category], e.DurationMS)
		if e.Outcome != "ok" {
			errorsBy[e.Category]++
		}
		if e.Source == "compact" || e.Category == "PostCompact" {
			signals++
			signalTasks[e.Task]++
		}
		if e.PrimeLoaded && e.CompactSignal != nil && e.Task != "" {
			loaded[e.Task] = true
		}
		if e.Coverage != nil {
			coverage++
			if e.Task != "" {
				latestCoverage[e.Task] = *e.Coverage
			}
		}
		if e.Task != "" && e.CheckpointAgeSeconds != nil {
			checkpointAges[e.Task] = *e.CheckpointAgeSeconds
		}
		if e.Feedback != "" {
			feedback[e.Feedback]++
			if e.CompactSignal != nil {
				linkedFeedback++
			}
		}
		if count == 1 {
			r["retained_since"] = e.At
		}
		r["last_observation_at"] = e.At
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	latency := map[string]any{}
	for name, values := range latencies {
		sort.Float64s(values)
		var sum float64
		for _, v := range values {
			sum += v
		}
		latency[name] = map[string]any{"count": len(values), "total_ms": sum, "p50_ms": values[(len(values)-1)/2], "p95_ms": values[(len(values)-1)*95/100]}
	}
	delete(signalTasks, "")
	hist := map[string]int{}
	for _, n := range signalTasks {
		bucket := "1"
		if n > 1 {
			bucket = "2-9"
		}
		if n >= 10 {
			bucket = "10+"
		}
		hist[bucket]++
	}
	pairs := 0
	pairMS := float64(0)
	for key, after := range post {
		if before, ok := pre[key]; ok {
			start, e1 := time.Parse(time.RFC3339Nano, before.At)
			end, e2 := time.Parse(time.RFC3339Nano, after.At)
			if e1 == nil && e2 == nil && !end.Before(start) {
				pairs++
				pairMS += float64(end.Sub(start)) / float64(time.Millisecond)
			}
		}
	}
	r["identified_compaction_turns_lower_bound"] = len(post)
	r["pre_post_turn_pairs_lower_bound"] = pairs
	r["pre_post_first_delivery_interval_ms_not_model_time"] = pairMS
	r["models_observed_allowlisted"] = models
	r["retention_cap_reached"] = evicted > 0
	r["storage_max_main_bytes"] = maxBytes
	r["observed_events"] = count
	r["observed_tasks"] = len(tasks)
	r["categories"] = categories
	r["errors"] = errorsBy
	r["latency"] = latency
	r["compact_signal_deliveries"] = signals
	r["tasks_with_compact_signals"] = len(signalTasks)
	r["signal_delivery_chain_histogram_not_unique_compactions"] = hist
	r["tasks_with_prime_after_compact_signal"] = len(loaded)
	totals := Coverage{}
	for _, c := range latestCoverage {
		totals.PendingUnits += c.PendingUnits
		totals.PendingBytes += c.PendingBytes
		totals.ReviewedUnits += c.ReviewedUnits
		totals.ReviewedBytes += c.ReviewedBytes
		totals.DeferredUnits += c.DeferredUnits
		totals.DeferredBytes += c.DeferredBytes
	}
	r["latest_observed_coverage_sum_not_current_store"] = totals
	r["tasks_with_coverage_observation"] = len(latestCoverage)
	var oldest int64
	for _, age := range checkpointAges {
		if age > oldest {
			oldest = age
		}
	}
	r["largest_checkpoint_age_seconds_at_last_observation"] = nil
	if len(checkpointAges) > 0 {
		r["largest_checkpoint_age_seconds_at_last_observation"] = oldest
	}
	r["coverage_observations"] = coverage
	r["coverage_unavailable_events"] = count - coverage
	r["feedback"] = feedback
	r["feedback_linked_to_signal_observation"] = linkedFeedback
	return r, nil
}

func SummaryCoverage(pendingUnits, pendingBytes, reviewedUnits, reviewedBytes, deferredUnits, deferredBytes int64) *Coverage {
	return &Coverage{pendingUnits, pendingBytes, reviewedUnits, reviewedBytes, deferredUnits, deferredBytes}
}
func CheckpointAge(at string) *int64 {
	t, e := time.Parse(time.RFC3339Nano, at)
	if e != nil {
		return nil
	}
	age := int64(time.Since(t).Seconds())
	if age < 0 {
		return nil
	}
	return &age
}
func Unavailable(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("telemetry unavailable (collection remains independent of memory)")
}
