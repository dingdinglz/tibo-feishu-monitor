package monitor

import (
	"context"
	"testing"
	"time"

	"tibo-monitor/internal/codexresets"
	"tibo-monitor/internal/model"
)

func TestScheduledIndependentOfHistoryAndSuppressesDebug(t *testing.T) {
	m, f, now := newResetTest(t)
	m.Config.DailyReport.Enabled = false
	m.Debug = true
	f.historyErr = &codexresets.Error{Code: "http_503", RetryAfter: time.Hour}
	f.status.ScheduledReset = &model.CodexScheduledReset{ID: "schedule", Status: "scheduled", ResetType: "banked", AnnouncedAt: now, Text: "coming", Source: model.CodexSource{Type: "observed"}}
	ctx := context.Background()
	out, err := m.Poll(ctx, now)
	if err != nil || out.Success || out.Added != 1 || out.RetryAfter != time.Hour {
		t.Fatal(out, err)
	}
	job, err := m.Store.Claim(ctx, "a", now, time.Second)
	if err != nil || job == nil || job.Kind != "scheduled" {
		t.Fatal(job, err)
	}
	f.historyErr = nil
	f.status.ScheduledReset.ID = "another-schedule"
	out, err = m.Poll(ctx, now.Add(time.Minute))
	st, statusErr := m.Store.Status(ctx)
	if err != nil || statusErr != nil || !out.Success || out.Added != 1 || st.Pending != 4 || f.statusCalls != 2 {
		t.Fatal("schedule duplicated debug or depended on history/daily", out, st, err, statusErr)
	}
	f.status.ScheduledReset = nil
	out, err = m.Poll(ctx, now.Add(2*time.Minute))
	if err != nil || !out.Success || out.Added != 0 {
		t.Fatal(out, err)
	}
}
