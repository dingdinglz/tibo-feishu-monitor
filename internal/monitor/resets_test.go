package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"tibo-monitor/internal/codexresets"
	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

type fakeResets struct {
	status                    model.CodexStatus
	resets                    []model.CodexReset
	err                       error
	statusErr, historyErr     error
	statusCalls, historyCalls int
	from                      []time.Time
}

func (f *fakeResets) Status(context.Context) (model.CodexStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return f.status, f.statusErr
	}
	return f.status, f.err
}
func (f *fakeResets) Resets(_ context.Context, from time.Time) ([]model.CodexReset, error) {
	f.historyCalls++
	f.from = append(f.from, from)
	if f.historyErr != nil {
		return f.resets, f.historyErr
	}
	return f.resets, f.err
}
func resetFixture(id string, at time.Time) model.CodexReset {
	return model.CodexReset{ID: id, ResetType: "regular", AnnouncedAt: at, Text: "Done", Source: model.CodexSource{Type: "observed"}}
}
func newResetTest(t *testing.T) (*Monitor, *fakeResets, time.Time) {
	m, _ := newTest(t)
	m.Config.Source = config.SourceCodexResets
	m.Config.DailyReport.Enabled = true
	now := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC) // 09:00 Shanghai
	f := &fakeResets{resets: []model.CodexReset{resetFixture("previous", now.Add(-72*time.Hour)), resetFixture("latest", now.Add(-3*time.Hour))}}
	avg := 2.875
	f.status = model.CodexStatus{LatestReset: &f.resets[1], Stats: model.CodexStats{Total: 2, AvgIntervalDays: &avg}, GeneratedAt: now.Add(-time.Minute)}
	m.ResetProvider = f
	return m, f, now
}

func TestCodexPollingBaselineCatchupAndSwitch(t *testing.T) {
	m, f, now := newResetTest(t)
	ctx := context.Background()
	// Simulate an existing X database. API state must not inherit its baseline.
	xState, err := m.Store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Poll(ctx, now)
	if err != nil || !out.Success || out.Added != 0 {
		t.Fatal(out, err)
	}
	st, _ := m.Store.Status(ctx)
	if st.Pending != 0 || f.from[0] != now.Add(-m.Config.Poll.CatchUpWindow) {
		t.Fatal("API baseline/catchup incorrect", st)
	}
	f.resets = append(f.resets, resetFixture("observed-new", now.Add(time.Minute)))
	out, err = m.Poll(ctx, now.Add(3*time.Minute))
	if err != nil || out.Added != 1 {
		t.Fatal(out, err)
	}
	out, err = m.Poll(ctx, now.Add(6*time.Minute))
	if err != nil || out.Added != 0 {
		t.Fatal("duplicate", out, err)
	}
	st, _ = m.Store.Status(ctx)
	xAfter, _ := m.Store.State(ctx)
	if st.Pending != 2 || xState != xAfter {
		t.Fatal("API polluted X state or fanout wrong", st)
	}
}

func TestHugeServerCooldownCannotOverflowJitter(t *testing.T) {
	maximum := time.Duration(1<<63 - 1)
	if jittered(maximum) != maximum || jittered(maximum-time.Second) < maximum-time.Second {
		t.Fatal("server cooldown overflowed into an immediate retry")
	}
}

func TestCodexFailureDoesNotInitializeAndPreservesRetryAfter(t *testing.T) {
	m, f, now := newResetTest(t)
	f.err = &codexresets.Error{Code: "rate_limited", RetryAfter: time.Hour}
	out, err := m.Poll(context.Background(), now)
	if err != nil || out.Success || out.RetryAfter != time.Hour {
		t.Fatal(out, err)
	}
	st, _ := m.Store.SourceState(context.Background(), config.SourceCodexResets)
	if st.Initialized {
		t.Fatal("failed fetch set baseline")
	}
}

func TestDailyDueTimezoneAndDST(t *testing.T) {
	for _, tc := range []struct {
		now, zone, at, date string
		due                 bool
	}{
		{"2026-10-15T00:59:59Z", "Asia/Shanghai", "09:00", "2026-10-15", false},
		{"2026-10-15T01:00:00Z", "Asia/Shanghai", "09:00", "2026-10-15", true},
		{"2026-10-15T16:01:00Z", "Asia/Shanghai", "00:00", "2026-10-16", true},
		{"2026-03-08T07:00:00Z", "America/New_York", "02:30", "2026-03-08", true},
		{"2026-11-01T05:30:00Z", "America/New_York", "01:30", "2026-11-01", true},
		{"2026-11-01T06:30:00Z", "America/New_York", "01:30", "2026-11-01", true},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		date, due, err := dailyDate(now, config.DailyReport{Time: tc.at, Timezone: tc.zone})
		if err != nil || date != tc.date || due != tc.due {
			t.Fatal(tc, date, due, err)
		}
	}
}

func TestDailySnapshotAndNoDuplicateAPIRequests(t *testing.T) {
	m, f, now := newResetTest(t)
	ctx := context.Background()
	out, err := m.QueueDailyReport(ctx, now.Add(-time.Second))
	if err != nil || out.Added != 0 || f.statusCalls != 0 {
		t.Fatal("sent before scheduled time", out, err)
	}
	out, err = m.QueueDailyReport(ctx, now)
	if err != nil || out.Added != 2 || !out.Success || !f.from[0].IsZero() {
		t.Fatal(out, err)
	}
	for i := 0; i < 2; i++ {
		out, err = m.QueueDailyReport(ctx, now.Add(time.Duration(i+1)*time.Hour))
		if err != nil || out.Added != 0 || f.statusCalls != 1 || f.historyCalls != 1 {
			t.Fatal("same day duplicate or needless API request", out, err)
		}
	}
	st, err := m.Store.Status(ctx)
	if err != nil || st.DailyReportDate != "2026-10-15" || st.Pending != 2 {
		t.Fatal(st, err)
	}
	report, err := BuildCodexReport(ctx, f, now, "Asia/Shanghai")
	if err != nil || report.LongestIntervalDays == nil || *report.LongestIntervalDays != 2.875 {
		t.Fatal("longest interval wrong", report, err)
	}
	// An endpoint cache skew must not produce a statistic from partial history.
	f.status.Stats.Total = 3
	report, err = BuildCodexReport(ctx, f, now, "Asia/Shanghai")
	if err != nil || report.LongestIntervalDays != nil {
		t.Fatal("computed longest from incomplete history", report, err)
	}
}

func TestDailyHistoryMustAgreeOnNewestRecordNotJustMembership(t *testing.T) {
	_, f, now := newResetTest(t)
	latest := *f.status.LatestReset
	previous := f.resets[0]
	// A removal plus a new record preserves total=2 and still contains B,
	// but status says B is latest while history's newest is C.
	f.resets = []model.CodexReset{latest, resetFixture("newer", now)}
	report, err := BuildCodexReport(context.Background(), f, now, "Asia/Shanghai")
	if err != nil || report.LongestIntervalDays != nil {
		t.Fatal("accepted stale status with same-count newer history", report, err)
	}
	amended := latest
	amended.AnnouncedAt = amended.AnnouncedAt.Add(-time.Hour)
	f.resets = []model.CodexReset{previous, amended}
	report, err = BuildCodexReport(context.Background(), f, now, "Asia/Shanghai")
	if err != nil || report.LongestIntervalDays != nil {
		t.Fatal("accepted mismatched timestamp for latest ID", report, err)
	}
}

func TestDailyFailureRetryDisabledAndX(t *testing.T) {
	m, f, now := newResetTest(t)
	ctx := context.Background()
	f.err = errors.New("do not expose upstream body")
	out, err := m.QueueDailyReport(ctx, now)
	if err != nil || out.Success || out.Added != 0 {
		t.Fatal(out, err)
	}
	needed, _ := m.Store.DailyReportNeeded(ctx, "2026-10-15")
	if !needed {
		t.Fatal("API failure consumed report date")
	}
	f.err = nil
	out, err = m.QueueDailyReport(ctx, now.Add(time.Minute))
	if err != nil || !out.Success || out.Added != 2 {
		t.Fatal("failed report wasn't retried", out, err)
	}
	calls := f.statusCalls
	m.Config.Source = config.SourceX
	out, err = m.QueueDailyReport(ctx, now.Add(24*time.Hour))
	if err != nil || out.Added != 0 || f.statusCalls != calls {
		t.Fatal("X source ran daily API reports", out, err)
	}
	m.Config.Source = config.SourceCodexResets
	m.Config.DailyReport.Enabled = false
	out, err = m.QueueDailyReport(ctx, now.Add(24*time.Hour))
	if err != nil || out.Added != 0 || f.statusCalls != calls {
		t.Fatal("disabled report ran", out, err)
	}
}

// The API poll can hang while the independent daily worker still creates and
// sends today's card. The fake's history calls distinguish full/window scans.
type blockingResetPoll struct {
	status model.CodexStatus
}

func (f blockingResetPoll) Status(context.Context) (model.CodexStatus, error) { return f.status, nil }
func (f blockingResetPoll) Resets(ctx context.Context, from time.Time) ([]model.CodexReset, error) {
	if from.IsZero() {
		return []model.CodexReset{}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestDailySchedulerIndependentOfPolling(t *testing.T) {
	m, _, _ := newResetTest(t)
	m.Config.DailyReport.Time = "00:00"
	m.ResetProvider = blockingResetPoll{status: model.CodexStatus{GeneratedAt: time.Now().UTC()}}
	delivered := make(chan string, 2)
	m.Sender = independentSender{delivered: delivered}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case id := <-delivered:
		if id != "b" {
			t.Fatal(id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daily scheduler blocked by API poll")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daily scheduler shutdown hung")
	}
}
