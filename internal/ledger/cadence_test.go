package ledger

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

type rootCadence interface {
	CompleteRootTurn(string) (int64, error)
	CapturePendingDebt() (PendingDebt, error)
	PendingDebtAfterRoot(PendingDebt) (PendingDebtStatus, error)
	ClaimStopContinuation(string, string) (bool, error)
}

func cadence(t *testing.T, l *Ledger) rootCadence {
	t.Helper()
	c, ok := any(l).(rootCadence)
	if !ok {
		t.Fatal("ledger does not implement real-root cadence")
	}
	return c
}
func TestRootTurnAgeAndPartialAcknowledgment(t *testing.T) {
	l := openTest(t, t.TempDir(), "cadence")
	c := cadence(t, l)
	r, err := l.CaptureV2(CaptureInput{Kind: "tool", Text: strings.Repeat("界", 2000), Key: "old", RootTurnID: "one"})
	check(t, err)
	_, err = l.CaptureV2(CaptureInput{Kind: "user", Text: "manual", Key: "manual"})
	check(t, err)
	for i, root := range []string{"one", "two", "three", "four"} {
		n, err := c.CompleteRootTurn(root)
		check(t, err)
		if n != int64(i+1) {
			t.Fatal(n)
		}
		again, err := c.CompleteRootTurn(root)
		check(t, err)
		if again != n {
			t.Fatal("duplicate advanced completion")
		}
		s, err := l.Status()
		check(t, err)
		if s.OldestPendingAgeTurns != int64(i) {
			t.Fatalf("age=%d want %d", s.OldestPendingAgeTurns, i)
		}
		if i == 1 {
			_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: []string{r.FirstUnitID}}))
			check(t, err)
		}
	}
	debt, err := c.CapturePendingDebt()
	check(t, err)
	_, err = l.CaptureV2(CaptureInput{Kind: "assistant", Text: strings.Repeat("tail", 20000), Key: "tail", RootTurnID: "four"})
	check(t, err)
	got, err := c.PendingDebtAfterRoot(debt)
	check(t, err)
	if got.Bytes != debt.Bytes || got.OldestAgeTurns != 3 {
		t.Fatalf("tail changed old debt: %+v snapshot %+v", got, debt)
	}
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{DeferSources: []SourceDeferral{{SourceID: r.SourceID, Reason: "large historical log"}}, Acknowledge: []string{}}))
	check(t, err)
	got, err = c.PendingDebtAfterRoot(debt)
	check(t, err)
	if got.Bytes != 6 || got.OldestAgeTurns != 3 {
		t.Fatalf("partial checkpoint reset remaining debt: %+v", got)
	}
	s, err := l.Status()
	check(t, err)
	if s.Coverage.Deferred.Bytes == 0 {
		t.Fatal("deferred coverage lost")
	}
}
func TestRootTurnDebtCheckpointRace(t *testing.T) {
	l := openTest(t, t.TempDir(), "cadence")
	c := cadence(t, l)
	_, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "old", Key: "old"})
	check(t, err)
	debt, err := c.CapturePendingDebt()
	check(t, err)
	_, err = l.ApplyV2(checkpointNow(t, l, CheckpointV2{Acknowledge: pendingIDs(t, l)}))
	check(t, err)
	_, err = l.CaptureV2(CaptureInput{Kind: "assistant", Text: strings.Repeat("new", 20000), Key: "new"})
	check(t, err)
	_, err = c.CompleteRootTurn("one")
	check(t, err)
	before, err := l.Status()
	check(t, err)
	got, err := c.PendingDebtAfterRoot(debt)
	check(t, err)
	after, err := l.Status()
	check(t, err)
	if got.Bytes != 0 || got.OldestAgeTurns != 0 || before.Through != after.Through || before.Revision != after.Revision {
		t.Fatalf("checkpoint race created debt or progress: %+v", got)
	}
}
func TestRootTurnAtomicCompletionAndClaim(t *testing.T) {
	l := openTest(t, t.TempDir(), "cadence")
	c := cadence(t, l)
	_, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "manual", Key: "manual"})
	check(t, err)
	_, err = l.db.Exec(`CREATE TRIGGER fail_anchor BEFORE UPDATE ON sources BEGIN SELECT RAISE(ABORT,'anchor failure'); END`)
	check(t, err)
	if _, err = c.CompleteRootTurn("one"); err == nil {
		t.Fatal("expected injected failure")
	}
	var count int
	check(t, l.db.QueryRow(`SELECT COUNT(*) FROM root_turns`).Scan(&count))
	if count != 0 {
		t.Fatal("completion escaped rollback")
	}
	n, err := l.State("root_completed_ordinal")
	check(t, err)
	if n != "0" {
		t.Fatal("counter escaped rollback")
	}
	_, err = l.db.Exec(`DROP TRIGGER fail_anchor`)
	check(t, err)
	_, err = c.CompleteRootTurn("one")
	check(t, err)
	_, err = l.db.Exec(`CREATE TRIGGER fail_claim BEFORE INSERT ON meta WHEN NEW.key='stop_prompt' BEGIN SELECT RAISE(ABORT,'claim failure'); END`)
	check(t, err)
	if _, err = c.ClaimStopContinuation("one", "exact prompt"); err == nil {
		t.Fatal("expected injected claim failure")
	}
	check(t, l.db.QueryRow(`SELECT continuation_claimed FROM root_turns WHERE id='one'`).Scan(&count))
	if count != 0 {
		t.Fatal("claim escaped rollback")
	}
	_, err = l.db.Exec(`DROP TRIGGER fail_claim`)
	check(t, err)
	won, err := c.ClaimStopContinuation("one", "exact prompt")
	check(t, err)
	if !won {
		t.Fatal("claim was lost")
	}
	won, err = c.ClaimStopContinuation("one", "different")
	check(t, err)
	if won {
		t.Fatal("claimed twice")
	}
	prompt, err := l.State("stop_prompt")
	check(t, err)
	parent, err := l.State("stop_turn")
	check(t, err)
	if prompt != "exact prompt" || parent != "one" {
		t.Fatal("claim metadata not atomic", prompt, parent)
	}
	if _, err = c.CompleteRootTurn(""); err == nil {
		t.Fatal("empty identity fabricated completion")
	}
	won, err = c.ClaimStopContinuation("missing", "prompt")
	check(t, err)
	if won {
		t.Fatal("claimed missing root")
	}
}
func TestRootTurnConcurrentCompletionAndClaim(t *testing.T) {
	store := t.TempDir()
	l := openTest(t, store, "cadence")
	cadence(t, l)
	const workers = 8
	handles := make([]*Ledger, workers)
	for i := range handles {
		handles[i] = openTest(t, store, "cadence")
	}
	ready := make(chan struct{})
	errors := make(chan error, workers)
	wins := make(chan bool, workers)
	var wg sync.WaitGroup
	for _, h := range handles {
		wg.Add(1)
		go func(h *Ledger) {
			defer wg.Done()
			<-ready
			c := any(h).(rootCadence)
			n, err := c.CompleteRootTurn("one")
			if err == nil && n != 1 {
				err = fmt.Errorf("ordinal=%d", n)
			}
			if err != nil {
				errors <- err
				return
			}
			won, err := c.ClaimStopContinuation("one", "exact")
			if err != nil {
				errors <- err
			}
			wins <- won
		}(h)
	}
	close(ready)
	wg.Wait()
	close(errors)
	close(wins)
	for err := range errors {
		t.Error(err)
	}
	count := 0
	for won := range wins {
		if won {
			count++
		}
	}
	if count != 1 {
		t.Fatal("winning claims", count)
	}
}

func TestRootTurnRoundTripImportAndInterruptedOrigin(t *testing.T) {
	for _, roundTrip := range []bool{false, true} {
		t.Run(fmt.Sprint(roundTrip), func(t *testing.T) {
			store := t.TempDir()
			l := openTest(t, store, "origin")
			c := cadence(t, l)
			_, err := l.CaptureV2(CaptureInput{Kind: "user", Text: "old uncompleted root evidence", Key: "old", RootTurnID: "old-root"})
			check(t, err)
			if roundTrip {
				_, err = l.Fork("fork")
				check(t, err)
				_, err = l.Import(store, "fork")
				check(t, err)
			}
			debt, err := c.CapturePendingDebt()
			check(t, err)
			for i := 0; i < 4; i++ {
				_, err = c.CompleteRootTurn(fmt.Sprintf("new-%d", i))
				check(t, err)
				got, err := c.PendingDebtAfterRoot(debt)
				check(t, err)
				if got.Bytes != debt.Bytes || got.OldestAgeTurns != int64(i) {
					t.Fatalf("unanchored recovery debt at boundary %d: %+v", i, got)
				}
			}
		})
	}
}
