package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/codexresets"
	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
)

func watchFixture(now time.Time) *model.CodexWatch {
	chance := 40
	return &model.CodexWatch{
		Level: "elevated", ResetChancePercent: &chance, ForecastWindow: "within 24h",
		ObservedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		Text: "possible reset", Source: model.CodexSource{Type: "observed"},
	}
}

func TestCodexWatchPolledWithoutDailyOrDebug(t *testing.T) {
	m, f, now := newResetTest(t)
	m.Config.DailyReport.Enabled = false
	f.status.ActiveWatch = watchFixture(now) // Announced at this run's startup boundary.
	ctx := context.Background()
	out, err := m.Poll(ctx, now)
	if err != nil || !out.Success || out.Added != 1 || f.statusCalls != 1 || f.historyCalls != 1 {
		t.Fatal("new watch requires daily report/debug or was baselined away", out, err)
	}
	for _, dest := range []string{"a", "b"} {
		job, err := m.Store.Claim(ctx, dest, now, time.Second)
		if err != nil || job == nil || job.Kind != "watch" || !strings.Contains(job.Text, `"reset_chance_percent":40`) {
			t.Fatal("watch was not queued separately", job, err)
		}
		if feishu.CardPost(job.Text) != nil {
			t.Fatal("watch entered TypeSafe post path")
		}
		if err := m.Store.Sent(ctx, *job, now); err != nil {
			t.Fatal(err)
		}
	}
	watch := f.status.ActiveWatch
	for i, active := range []*model.CodexWatch{watch, nil, watch} {
		f.status.ActiveWatch = active
		f.status.GeneratedAt = now.Add(time.Duration(i+1) * time.Minute)
		out, err = m.Poll(ctx, now.Add(time.Duration(i+1)*time.Minute))
		if err != nil || !out.Success || out.Added != 0 {
			t.Fatal("same/reappearing watch duplicated", out, err)
		}
	}
	f.status.ActiveWatch.ObservedAt = now.Add(time.Minute)
	out, err = m.Poll(ctx, now.Add(5*time.Minute))
	if err != nil || !out.Success || out.Added != 1 {
		t.Fatal("new watch observation was suppressed", out, err)
	}
	st, err := m.Store.Status(ctx)
	if err != nil || st.Sent != 2 || st.Pending != 2 || st.DailyReportDate != "" {
		t.Fatal("watch fanout changed daily bookkeeping", st, err)
	}
}

func TestCodexWatchExpiredAndScheduledAreNotConfirmedResets(t *testing.T) {
	m, f, now := newResetTest(t)
	f.status.ActiveWatch = watchFixture(now.Add(-time.Hour))
	f.status.ActiveWatch.ExpiresAt = now
	past := now.Add(-time.Hour)
	f.status.ScheduledReset = &model.CodexScheduledReset{
		ID: "pending", Status: "scheduled", ResetType: "regular", AnnouncedAt: now,
		ScheduledFor: &past, Source: model.CodexSource{Type: "observed"},
	}
	out, err := m.Poll(context.Background(), now)
	st, statusErr := m.Store.Status(context.Background())
	if err != nil || statusErr != nil || !out.Success || out.Added != 1 || st.Pending != 2 {
		t.Fatal("expired watch notified or scheduled announcement was lost", out, st, err, statusErr)
	}
	job, err := m.Store.Claim(context.Background(), "a", now, time.Second)
	if err != nil || job == nil || job.Kind != "scheduled" {
		t.Fatal("past schedule became a confirmed reset", job, err)
	}
}

func TestCodexWatchSuppressesDebugAndReusesStatus(t *testing.T) {
	m, f, now := newResetTest(t)
	m.Debug = true
	f.status.ActiveWatch = watchFixture(now)
	ctx := context.Background()
	out, err := m.Poll(ctx, now)
	st, _ := m.Store.Status(ctx)
	if err != nil || out.Added != 1 || st.Pending != 2 || f.statusCalls != 1 {
		t.Fatal("watch triggered redundant debug/status request", out, st, err)
	}
	out, err = m.Poll(ctx, now.Add(3*time.Minute))
	st, _ = m.Store.Status(ctx)
	if err != nil || !out.Success || out.Added != 0 || st.Pending != 4 || f.statusCalls != 2 {
		t.Fatal("unchanged watch broke ordinary debug replay", out, st, err)
	}
}

func TestCodexStatusFailureDoesNotLoseResetsOrWatches(t *testing.T) {
	m, f, now := newResetTest(t)
	ctx := context.Background()
	if out, err := m.Poll(ctx, now); err != nil || !out.Success {
		t.Fatal(out, err)
	}
	f.resets = append(f.resets, resetFixture("new", now.Add(time.Minute)))
	f.status.ActiveWatch = watchFixture(now)
	f.statusErr = &codexresets.Error{Code: "http_429", RetryAfter: time.Hour}
	out, err := m.Poll(ctx, now.Add(3*time.Minute))
	st, _ := m.Store.Status(ctx)
	if err != nil || out.Success || out.Added != 1 || out.RetryAfter != time.Hour || st.Pending != 2 {
		t.Fatal("status failure lost a confirmed reset or cooldown", out, st, err)
	}
	f.statusErr = nil
	out, err = m.Poll(ctx, now.Add(6*time.Minute))
	st, _ = m.Store.Status(ctx)
	if err != nil || !out.Success || out.Added != 1 || st.Pending != 4 {
		t.Fatal("recovery lost watch or duplicated confirmed reset", out, st, err)
	}
}

func TestCodexHistoryFailureDoesNotHideActiveWatch(t *testing.T) {
	m, f, now := newResetTest(t)
	ctx := context.Background()
	f.historyErr = &codexresets.Error{Code: "http_503", RetryAfter: time.Hour}
	f.status.ActiveWatch = watchFixture(now)
	out, err := m.Poll(ctx, now)
	state, _ := m.Store.SourceState(ctx, config.SourceCodexResets)
	st, _ := m.Store.Status(ctx)
	if err != nil || out.Success || out.Added != 1 || out.RetryAfter != time.Hour || st.Pending != 2 || state.Initialized {
		t.Fatal("history failure hid watch or advanced reset baseline", out, st, err)
	}
	f.statusErr = &codexresets.Error{Code: "http_429", RetryAfter: 2 * time.Hour}
	out, err = m.Poll(ctx, now.Add(time.Minute))
	if err != nil || out.Success || out.RetryAfter != 2*time.Hour {
		t.Fatal("combined endpoint cooldown lost", out, err)
	}
	f.statusErr, f.historyErr = nil, nil
	out, err = m.Poll(ctx, now.Add(3*time.Minute))
	if err != nil || !out.Success || out.Added != 0 {
		t.Fatal("recovery replayed watch", out, err)
	}
}
