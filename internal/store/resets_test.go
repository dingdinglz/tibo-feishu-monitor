package store

import (
	"errors"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

func apiReset(id string, at time.Time) model.CodexReset {
	return model.CodexReset{ID: id, AnnouncedAt: at, ResetType: "regular", Text: "reset", Source: model.CodexSource{Type: "observed"}}
}
func resetFormat(r model.CodexReset, debug bool) (string, error) { return r.ID, nil }

func TestCodexStateAndDedupSeparateFromX(t *testing.T) {
	s, path := openTest(t)
	apply(t, s, post("42", epoch.Add(time.Second)))
	resets := []model.CodexReset{apiReset("old", epoch.Add(-time.Hour)), apiReset("42", epoch.Add(time.Second))}
	n, err := s.ApplyCodexScan(ctx, resets, state(), epoch.Add(time.Hour), epoch.Add(-time.Hour), resetFormat)
	if err != nil || n != 1 || pending(t, s) != 6 {
		t.Fatal("reset sharing an X ID was lost", n, err)
	}
	if err := s.SetSource(ctx, config.SourceCodexResets); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "new"}}); err != nil {
		t.Fatal(err)
	}
	n, err = s.ApplyCodexScan(ctx, resets, state(), epoch.Add(2*time.Hour), epoch.Add(-time.Hour), resetFormat)
	if err != nil || n != 0 || pending(t, s) != 6 {
		t.Fatal("restart/new target replayed history", n, err)
	}
	x, _ := s.State(ctx)
	api, _ := s.SourceState(ctx, config.SourceCodexResets)
	st, err := s.Status(ctx)
	if err != nil || !x.Initialized || !api.Initialized || st.Source != config.SourceCodexResets {
		t.Fatal("source state lost", st, err)
	}
}

func TestCodexScanRollback(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.ApplyCodexScan(ctx, []model.CodexReset{apiReset("42", epoch)}, state(), epoch, epoch,
		func(model.CodexReset, bool) (string, error) { return "", errors.New("format") })
	if err == nil {
		t.Fatal("expected formatter error")
	}
	st, _ := s.SourceState(ctx, config.SourceCodexResets)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM codex_resets").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || st.Initialized || pending(t, s) != 0 {
		t.Fatal("failed scan committed")
	}
}

func TestDailyReportDurablePerDestinationAndSuperseding(t *testing.T) {
	s, path := openTest(t)
	date := "2026-01-01"
	needed, err := s.DailyReportNeeded(ctx, date)
	if err != nil || !needed {
		t.Fatal(needed, err)
	}
	n, err := s.QueueDailyReport(ctx, date, "snapshot one", epoch)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	job, err := s.Claim(ctx, "a", epoch, time.Second)
	if err != nil || job == nil || job.Kind != "daily_report" {
		t.Fatal(job, err)
	}
	if err := s.Sent(ctx, *job, epoch); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, err := s.QueueDailyReport(ctx, date, "should not replace", epoch.Add(time.Hour)); err != nil || n != 0 {
		t.Fatal("restart duplicate", n, err)
	}
	if needed, err := s.DailyReportNeeded(ctx, date); err != nil || needed {
		t.Fatal("queued or sent day should be consumed", needed, err)
	}
	job, err = s.Claim(ctx, "b", epoch.Add(time.Hour), time.Second)
	if err != nil || job == nil || job.Text != "snapshot one" {
		t.Fatal("snapshot not durable", job, err)
	}
	if err := s.Failed(ctx, *job, epoch.Add(2*time.Hour), "transport", false); err != nil {
		t.Fatal(err)
	}
	n, err = s.QueueDailyReport(ctx, "2026-01-02", "snapshot two", epoch.Add(24*time.Hour))
	if err != nil || n != 2 || pending(t, s) != 2 {
		t.Fatal("old daily backlog was not superseded", n, err)
	}
	job, err = s.Claim(ctx, "b", epoch.Add(25*time.Hour), time.Second)
	if err != nil || job == nil || job.Text != "snapshot two" {
		t.Fatal(job, err)
	}
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "new"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueDailyReport(ctx, "2026-01-02", "new target", epoch.Add(26*time.Hour)); err != nil || n != 1 {
		t.Fatal("new daily target or disabled target handling", n, err)
	}
}

func TestCodexDebugDoesNotChangeScanOrAccumulate(t *testing.T) {
	s, _ := openTest(t)
	r := apiReset("observed-test", epoch)
	for i, want := range []int{2, 0} {
		n, err := s.QueueCodexDebug(ctx, r, epoch.Add(time.Duration(i)*time.Minute), resetFormat)
		if err != nil || n != want {
			t.Fatal(n, err)
		}
	}
	st, _ := s.SourceState(ctx, config.SourceCodexResets)
	if st.Initialized {
		t.Fatal("debug changed baseline")
	}
}
