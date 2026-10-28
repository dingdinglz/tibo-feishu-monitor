package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/store"
)

func TestStartupBoundaryIncludesEventsBeforeFirstSuccessfulPoll(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{config.SourceX, config.SourceCodexResets} {
		t.Run(source, func(t *testing.T) {
			m, api, start := newResetTest(t)
			m.Config.Source, m.StartedAt = source, start
			if source == config.SourceX {
				f := &fakeFetch{err: errors.New("temporary")}
				m.Fetcher = f
				if out, err := m.Poll(ctx, start.Add(time.Minute)); err != nil || out.Success {
					t.Fatal(out, err)
				}
				f.err = nil
				f.result = model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{fixture("1", start.Add(-time.Second)), fixture("2", start.Add(time.Second))}}
			} else {
				api.err = errors.New("temporary")
				if out, err := m.Poll(ctx, start.Add(time.Minute)); err != nil || out.Success {
					t.Fatal(out, err)
				}
				api.err = nil
				api.resets = []model.CodexReset{resetFixture("1", start.Add(-time.Second)), resetFixture("2", start.Add(time.Second))}
			}
			out, err := m.Poll(ctx, start.Add(2*time.Minute))
			if err != nil || !out.Success || out.Added != 1 {
				t.Fatal("startup-to-first-fetch event lost", out, err)
			}
			job, err := m.Store.Claim(ctx, "a", start.Add(3*time.Minute), time.Second)
			if err != nil || job == nil || !strings.Contains(job.DedupKey, ":2:") {
				t.Fatal(job, err)
			}
		})
	}
}

func TestRestartSkipsOldQueueAndNewlyDiscoveredHistory(t *testing.T) {
	ctx := context.Background()
	m, f := newTest(t)
	start := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	m.StartedAt = start
	f.result = model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{fixture("1", start.Add(time.Second))}}
	if out, err := m.Poll(ctx, start.Add(time.Minute)); err != nil || out.Added != 1 {
		t.Fatal(out, err)
	}
	restart := start.Add(time.Hour)
	f.result.Posts = append(f.result.Posts, fixture("2", restart.Add(-time.Minute)), fixture("3", restart.Add(time.Second)))
	next := &Monitor{Config: m.Config, Store: m.Store, Fetcher: f, Log: m.Log, StartedAt: restart}
	if out, err := next.Poll(ctx, restart.Add(time.Minute)); err != nil || out.Added != 1 {
		t.Fatal(out, err)
	}
	job, err := next.Store.Claim(ctx, "a", restart.Add(time.Minute), time.Second)
	if err != nil || job == nil || !strings.Contains(job.DedupKey, ":3:") {
		t.Fatal("old queue leaked or blocked new post", job, err)
	}
	if err := next.Store.Sent(ctx, *job, restart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if job, err := next.Store.Claim(ctx, "a", restart.Add(2*time.Minute), time.Second); err != nil || job != nil {
		t.Fatal("startup history leaked", job, err)
	}
}

func TestExplicitSinceOverridesBaselineCatchupAndStoredDiscoveries(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{config.SourceX, config.SourceCodexResets} {
		t.Run(source, func(t *testing.T) {
			m, api, start := newResetTest(t)
			m.Config.Source = source
			since := start.Add(-72 * time.Hour)
			f := &fakeFetch{result: model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{fixture("10", since.Add(time.Minute))}}}
			m.Fetcher = f
			api.resets = []model.CodexReset{resetFixture("10", since.Add(time.Minute))}
			if out, err := m.Poll(ctx, start); err != nil || out.Added != 0 {
				t.Fatal(out, err)
			}
			next := &Monitor{Config: m.Config, Store: m.Store, Fetcher: f, ResetProvider: api, Log: m.Log, StartedAt: start.Add(time.Hour), SendSince: since}
			if out, err := next.Poll(ctx, start.Add(time.Hour)); err != nil || out.Added != 1 {
				t.Fatal("explicit history did not override baseline/dedup", out, err)
			}
			if source == config.SourceX && !f.requests[len(f.requests)-1].Since.Equal(since) {
				t.Fatal("X history was limited by catch-up")
			}
			if source == config.SourceCodexResets && !api.from[len(api.from)-1].Equal(since) {
				t.Fatal("API history was limited by catch-up")
			}
			if out, err := next.Poll(ctx, start.Add(2*time.Hour)); err != nil || out.Added != 0 {
				t.Fatal("explicit history duplicated", out, err)
			}
		})
	}
}

func TestRestartResetsLegacyCursorForExplicitHistory(t *testing.T) {
	m, f := newTest(t)
	ctx := context.Background()
	start := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	oldState := store.ScanState{Initialized: true, Baseline: start, Cursor: "old-window", ScanStarted: start, ScanSince: start}
	if _, err := m.Store.ApplyScan(ctx, model.FetchResult{}, oldState, start, start, feishu.Format); err != nil {
		t.Fatal(err)
	}
	m.StartedAt, m.SendSince = start.Add(time.Hour), start.Add(-24*time.Hour)
	f.err = errors.New("temporary")
	if _, err := m.Poll(ctx, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.err = nil
	f.result = model.FetchResult{HeadOK: true, Complete: false, Cursor: "new-window"}
	if _, err := m.Poll(ctx, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Poll(ctx, start.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if f.requests[0].Cursor != "" || f.requests[1].Cursor != "" || f.requests[2].Cursor != "new-window" {
		t.Fatal("old startup cursor reused or new continuation lost", f.requests)
	}
	for _, r := range f.requests {
		if !r.Since.Equal(m.SendSince) {
			t.Fatal(r)
		}
	}
}

func TestScheduledAndWatchUseAnnouncementNotFuturePlanOrExpiry(t *testing.T) {
	ctx := context.Background()
	m, f, start := newResetTest(t)
	future := start.Add(time.Hour)
	f.status.ScheduledReset = &model.CodexScheduledReset{ID: "old-plan", Status: "scheduled", ResetType: "regular", AnnouncedAt: start.Add(-time.Second), ScheduledFor: &future, Source: model.CodexSource{Type: "observed"}}
	f.status.ActiveWatch = watchFixture(start.Add(-time.Second))
	if out, err := m.Poll(ctx, start); err != nil || out.Added != 0 {
		t.Fatal("old active state leaked", out, err)
	}
	f.status.ScheduledReset.AnnouncedAt = start
	f.status.ActiveWatch.ObservedAt = start
	if out, err := m.Poll(ctx, start.Add(time.Minute)); err != nil || out.Added != 2 {
		t.Fatal("filtered state consumed new event identity", out, err)
	}
}

func TestDebugExceptionDoesNotEnableOrdinaryHistory(t *testing.T) {
	m, f, start := newResetTest(t)
	m.Debug = true
	future := start.Add(time.Hour)
	f.status.ScheduledReset = &model.CodexScheduledReset{ID: "old-plan", Status: "scheduled", ResetType: "regular", AnnouncedAt: start.Add(-time.Hour), ScheduledFor: &future, Source: model.CodexSource{Type: "observed"}}
	f.status.ActiveWatch = watchFixture(start.Add(-time.Hour))
	ctx := context.Background()
	out, err := m.Poll(ctx, start)
	if err != nil || out.Added != 0 {
		t.Fatal(out, err)
	}
	job, err := m.Store.Claim(ctx, "a", start, time.Second)
	if err != nil || job == nil || job.Kind != "debug" || !strings.Contains(job.Text, "latest") {
		t.Fatal(job, err)
	}
	st, err := m.Store.Status(ctx)
	if err != nil || st.Pending != 2 {
		t.Fatal("debug allowed ordinary history", st, err)
	}
}

func TestRunFiltersOldQueueBeforeWorkersStart(t *testing.T) {
	m, _ := newTest(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Millisecond)
	m.StartedAt = start
	old, fresh := fixture("11", start.Add(-time.Hour)), fixture("12", start.Add(time.Millisecond))
	if _, err := m.Store.ApplyScan(ctx, model.FetchResult{Posts: []model.Post{old, fresh}}, store.ScanState{Initialized: true, Baseline: old.CreatedAt}, start, old.CreatedAt, feishu.Format); err != nil {
		t.Fatal(err)
	}
	m.Fetcher = blockingFetch{}
	m.Config.Webhooks = m.Config.Webhooks[:1]
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sender := &captureSend{cancel: cancel}
	m.Sender = sender
	if err := m.Run(runCtx); err != nil {
		t.Fatal(err)
	}
	p := feishu.CardPost(sender.body)
	if p == nil || p.ID != fresh.ID {
		t.Fatal("worker sent old backlog before first poll", sender.body)
	}
}

func TestDailyStartupSkipAndExplicitSince(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		m, f, due := newResetTest(t)
		m.StartedAt = due.Add(time.Minute)
		if explicit {
			m.SendSince = due.Add(-time.Hour)
		}
		out, err := m.QueueDailyReport(context.Background(), due.Add(2*time.Minute))
		want := 0
		if explicit {
			want = 2
		}
		if err != nil || out.Added != want {
			t.Fatal(explicit, out, err)
		}
		if !explicit && f.statusCalls != 0 {
			t.Fatal("skipped daily fetched API")
		}
		out, err = m.QueueDailyReport(context.Background(), due.Add(24*time.Hour))
		if err != nil || out.Added != 2 {
			t.Fatal("next daily was skipped", out, err)
		}
	}
}

func TestDailyStartupDSTFirstOccurrenceAndGap(t *testing.T) {
	for _, tc := range []struct{ now, zone, at, want string }{
		{"2026-11-01T06:30:00Z", "America/New_York", "01:30", "2026-11-01T05:30:00Z"},
		{"2026-03-08T07:00:00Z", "America/New_York", "02:30", "2026-03-08T07:00:00Z"},
		{"2021-10-28T22:30:00Z", "Asia/Amman", "00:30", "2021-10-28T21:30:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		got, err := dailyScheduledAt(now, config.DailyReport{Timezone: tc.zone, Time: tc.at})
		if err != nil || got.UTC().Format(time.RFC3339) != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	m, f, _ := newResetTest(t)
	m.Config.DailyReport.Timezone, m.Config.DailyReport.Time = "America/New_York", "01:30"
	m.StartedAt, _ = time.Parse(time.RFC3339, "2026-11-01T06:15:00Z") // After first 01:30, before repeated 01:30.
	now, _ := time.Parse(time.RFC3339, "2026-11-01T06:30:00Z")
	out, err := m.QueueDailyReport(context.Background(), now)
	if err != nil || out.Added != 0 || f.statusCalls != 0 {
		t.Fatal("DST repetition replayed pre-start schedule", out, err)
	}
}
