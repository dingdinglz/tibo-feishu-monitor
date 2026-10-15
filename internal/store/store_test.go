package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

var ctx = context.Background()
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func format(p model.Post) ([]string, error)   { return []string{p.Text + " /1", p.Text + " /2"}, nil }
func post(id string, at time.Time) model.Post { return model.Post{ID: id, CreatedAt: at, Text: id} }
func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "monitor.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	return s, p
}
func state() ScanState { return ScanState{Initialized: true, Baseline: epoch, LastComplete: epoch} }
func apply(t *testing.T, s *Store, ps ...model.Post) {
	t.Helper()
	if _, err := s.ApplyScan(ctx, model.FetchResult{Posts: ps}, state(), epoch.Add(time.Hour), epoch.Add(-time.Hour), format); err != nil {
		t.Fatal(err)
	}
}
func pending(t *testing.T, s *Store) int {
	t.Helper()
	st, e := s.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return st.Pending
}

func TestAtomicDedupBaselineAndNewDestination(t *testing.T) {
	s, _ := openTest(t)
	old, fresh := post("1", epoch.Add(-time.Second)), post("2", epoch.Add(time.Second))
	apply(t, s, old, fresh)
	if pending(t, s) != 4 {
		t.Fatal("baseline or fanout incorrect")
	}
	apply(t, s, old, fresh)
	if pending(t, s) != 4 {
		t.Fatal("duplicates queued")
	}
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
		t.Fatal(err)
	}
	apply(t, s, fresh)
	if pending(t, s) != 4 {
		t.Fatal("new target received history")
	}
	apply(t, s, post("3", epoch.Add(2*time.Second)))
	if pending(t, s) != 10 {
		t.Fatal("new target missed future post")
	}
}

func TestFailureIsolationRestartAndPause(t *testing.T) {
	s, p := openTest(t)
	apply(t, s, post("2", epoch.Add(time.Second)))
	j, err := s.Claim(ctx, "a", epoch.Add(time.Hour), time.Second)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	if err = s.Failed(ctx, *j, epoch.Add(2*time.Hour), "temporary", false); err != nil {
		t.Fatal(err)
	}
	b, err := s.Claim(ctx, "b", epoch.Add(time.Hour), time.Second)
	if err != nil || b == nil {
		t.Fatal("one target blocked another", err)
	}
	if err = s.Sent(ctx, *b, epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err = s2.SyncDestinations(ctx, "target", []config.Destination{{ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	if j, err = s2.Claim(ctx, "a", epoch.Add(3*time.Hour), time.Second); j != nil || err != nil {
		t.Fatal("removed destination not paused", err)
	}
	if err = s2.SyncDestinations(ctx, "target", []config.Destination{{ID: "a", URL: "new secret URL"}, {ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	j, err = s2.Claim(ctx, "a", epoch.Add(3*time.Hour), time.Second)
	if err != nil || j == nil || j.Attempts != 2 {
		t.Fatal("retry not restored", j, err)
	}
	st, err := s2.State(ctx)
	if err != nil || !st.Initialized || !st.Baseline.Equal(epoch) {
		t.Fatal("restart reset baseline", err)
	}
}

func TestRollbackOnFormattingFailure(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.ApplyScan(ctx, model.FetchResult{Posts: []model.Post{post("1", epoch)}}, state(), epoch, epoch, func(model.Post) ([]string, error) { return nil, errors.New("format") })
	if err == nil {
		t.Fatal("expected rollback")
	}
	st, err := s.Status(ctx)
	if err != nil || st.Posts != 0 || st.Scan.Initialized {
		t.Fatal(st, err)
	}
	apply(t, s, post("1", epoch))
	if pending(t, s) != 4 {
		t.Fatal("rollback lost post")
	}
}

func TestClaimCrashAndOrder(t *testing.T) {
	s, p := openTest(t)
	apply(t, s, post("11", epoch.Add(2*time.Second)), post("10", epoch.Add(time.Second)))
	j, err := s.Claim(ctx, "a", epoch.Add(time.Minute), time.Second)
	if err != nil || j == nil || j.Text != "10 /1" {
		t.Fatal(j, err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	retry, err := s2.Claim(ctx, "a", epoch.Add(2*time.Minute), time.Second)
	if err != nil || retry == nil || retry.ID != j.ID || retry.Attempts != 2 {
		t.Fatal("unacknowledged claim lost", retry, err)
	}
	if err = s2.Sent(ctx, *retry, epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	next, err := s2.Claim(ctx, "a", epoch.Add(3*time.Minute), time.Second)
	if err != nil || next.Text != "10 /2" {
		t.Fatal("fragment order", next, err)
	}
}

func TestFaultThresholdCooldownAndRecovery(t *testing.T) {
	s, _ := openTest(t)
	for i := 0; i < 2; i++ {
		if err := s.Fault(ctx, "scrape", "failure", "", epoch, 3, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if pending(t, s) != 0 {
		t.Fatal("alert before threshold")
	}
	if err := s.Fault(ctx, "scrape", "failure", "", epoch, 3, time.Hour); err != nil {
		t.Fatal(err)
	}
	if pending(t, s) != 2 {
		t.Fatal("alert missing")
	}
	if err := s.Fault(ctx, "scrape", "failure", "", epoch.Add(time.Minute), 3, time.Hour); err != nil {
		t.Fatal(err)
	}
	if pending(t, s) != 2 {
		t.Fatal("alert storm")
	}
	if err := s.Recover(ctx, "scrape", "ok", epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, "scrape", "ok", epoch.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if pending(t, s) != 4 {
		t.Fatal("recovery duplicated or missing")
	}
}

func TestCooldownAlsoBlocksAlertsAndPermanentUntilRestart(t *testing.T) {
	s, _ := openTest(t)
	apply(t, s, post("1", epoch))
	j, _ := s.Claim(ctx, "a", epoch.Add(time.Minute), time.Second)
	if err := s.Failed(ctx, *j, epoch.Add(time.Hour), "rate_limit", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Fault(ctx, "coverage", "gap", "", epoch.Add(time.Minute), 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Claim(ctx, "a", epoch.Add(2*time.Minute), time.Second); got != nil || err != nil {
		t.Fatal("alert bypassed cooldown", err)
	}
	if err := s.Failed(ctx, *j, epoch, "signature", true); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Claim(ctx, "a", epoch.Add(2*time.Hour), time.Second); got != nil || err != nil {
		t.Fatal("permanent failure retried", err)
	}
	if err := s.SyncDestinations(ctx, "different target", nil); err == nil {
		t.Fatal("target mismatch accepted")
	}
}
