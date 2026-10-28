package monitor

import (
	"context"
	"errors"
	"sort"
	"time"

	"tibo-monitor/internal/codexresets"
	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
)

type ResetProvider interface {
	Status(context.Context) (model.CodexStatus, error)
	Resets(context.Context, time.Time) ([]model.CodexReset, error)
}

func (m *Monitor) resetFailure(ctx context.Context, component string, fetchErr error) (PollResult, error) {
	var outcome PollResult
	if ctx.Err() != nil {
		return outcome, ctx.Err()
	}
	var apiErr *codexresets.Error
	if errors.As(fetchErr, &apiErr) {
		outcome.RetryAfter = apiErr.RetryAfter
	}
	// Upstream bodies and arbitrary error strings never reach logs or cards.
	err := m.fault(ctx, component, "Codex Resets API 部分数据暂不可用，将稍后重试；已入队的通知继续发送。", "", m.Config.Alerts.FailureThreshold)
	return outcome, err
}

func (m *Monitor) pollResets(ctx context.Context, now time.Time) (PollResult, error) {
	var outcome PollResult
	state, err := m.Store.SourceState(ctx, config.SourceCodexResets)
	if err != nil {
		return outcome, err
	}
	if m.ResetProvider == nil {
		return outcome, errors.New("codex-resets provider is not configured")
	}
	cutoff := m.scanCutoff(now)
	last := state.LastComplete
	if last.Before(m.StartedAt) {
		last = m.StartedAt
	}
	if state.Initialized && last.Before(cutoff) {
		if err := m.fault(ctx, "codex_window", "距离上次 API 采集已超过补抓窗口，窗口外的重置记录不再自动补发。", "", 1); err != nil {
			return outcome, err
		}
	}
	resets, historyErr := m.ResetProvider.Resets(ctx, cutoff)
	if historyErr != nil {
		outcome, err = m.resetFailure(ctx, "codex_resets", historyErr)
		if err != nil {
			return outcome, err
		}
	} else {
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		if !state.Initialized {
			state.Initialized = true
			m.Log.Info("baseline_established", "source", config.SourceCodexResets, "since", m.deliverySince())
		}
		state.Baseline = m.deliverySince()
		state.LastComplete, state.LastRequestSuccess = now, now
		outcome.Added, err = m.Store.ApplyCodexScan(ctx, resets, state, now, cutoff, feishu.FormatCodexReset)
		if err != nil {
			return outcome, err
		}
		if err := m.Store.Recover(ctx, "codex_resets", "Codex Resets API 采集已恢复。", now); err != nil {
			return outcome, err
		}
	}
	newResets := outcome.Added
	// Status is polled even without daily reports/debug. A failed history fetch
	// must not hide a live watch, and a failed status fetch must not drop resets.
	status, statusErr := m.ResetProvider.Status(ctx)
	if statusErr != nil {
		failure, err := m.resetFailure(ctx, "codex_status", statusErr)
		outcome.RetryAfter = max(outcome.RetryAfter, failure.RetryAfter)
		return outcome, err
	}
	newScheduled, err := m.Store.QueueCodexScheduledReset(ctx, status.ScheduledReset, feishu.FormatCodexScheduledReset)
	if err != nil {
		return outcome, err
	}
	outcome.Added += newScheduled
	newWatches, err := m.Store.QueueCodexWatch(ctx, status.ActiveWatch, now, feishu.FormatCodexWatch)
	if err != nil {
		return outcome, err
	}
	outcome.Added += newWatches
	if err := m.Store.Recover(ctx, "codex_status", "Codex Resets 状态、计划及预测读取已恢复。", now); err != nil {
		return outcome, err
	}
	if m.Debug && historyErr == nil && outcome.Added == 0 {
		if status.LatestReset != nil {
			n, err := m.Store.QueueCodexDebug(ctx, *status.LatestReset, now, feishu.FormatCodexReset)
			if err != nil {
				return outcome, err
			}
			m.Log.Info("debug_latest_queued", "source", config.SourceCodexResets, "destinations", n)
		}
		if err := m.Store.Recover(ctx, "codex_debug", "API 调试状态读取已恢复。", now); err != nil {
			return outcome, err
		}
	}
	outcome.Success = historyErr == nil
	m.Log.Info("poll_finished", "source", config.SourceCodexResets, "new_resets", newResets, "new_scheduled", newScheduled, "new_watches", newWatches)
	return outcome, nil
}

// Calendar-day comparison also handles DST: repeated wall-clock times dedup by
// date; a skipped scheduled time becomes due at the next valid wall-clock time.
func dailyDate(now time.Time, cfg config.DailyReport) (string, bool, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return "", false, errors.New("invalid daily report timezone")
	}
	at, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return "", false, errors.New("invalid daily report time")
	}
	local := now.In(loc)
	return local.Format("2006-01-02"), local.Hour()*60+local.Minute() >= at.Hour()*60+at.Minute(), nil
}

// BuildCodexReport fetches a fresh snapshot without mutating storage or sending.
// Only complete, matching history is used for the derived longest interval.
func BuildCodexReport(ctx context.Context, provider ResetProvider, now time.Time, timezone string) (model.CodexReport, error) {
	var report model.CodexReport
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return report, errors.New("invalid report timezone")
	}
	status, err := provider.Status(ctx)
	if err != nil {
		return report, err
	}
	resets, err := provider.Resets(ctx, time.Time{})
	if err != nil {
		return report, err
	}
	report = model.CodexReport{Status: status, AsOf: now, Date: now.In(loc).Format("2006-01-02"), Timezone: timezone}
	if len(resets) == status.Stats.Total && len(resets) > 1 {
		times := make([]time.Time, 0, len(resets))
		seen := make(map[string]bool, len(resets))
		latestMatches := false
		for _, r := range resets {
			if seen[r.ID] || !r.Valid() {
				return model.CodexReport{}, errors.New("invalid reset history")
			}
			seen[r.ID] = true
			times = append(times, r.AnnouncedAt)
			if latest := status.LatestReset; latest != nil && r.ID == latest.ID &&
				r.AnnouncedAt.Equal(latest.AnnouncedAt) && r.ResetType == latest.ResetType {
				latestMatches = true
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		// Membership alone is insufficient: a removal and a new reset can
		// preserve the count while the old status.latest_reset is still present.
		if latestMatches && !times[len(times)-1].After(status.LatestReset.AnnouncedAt) {
			longest := 0.0
			for i := 1; i < len(times); i++ {
				longest = max(longest, times[i].Sub(times[i-1]).Hours()/24)
			}
			report.LongestIntervalDays = &longest
		}
	}
	if !report.Valid() {
		return model.CodexReport{}, errors.New("invalid reset status")
	}
	return report, nil
}

// QueueDailyReport can be safely called after restart or repeatedly on the same
// date. API failures do not consume the day's slot; the outbox owns retries.
func (m *Monitor) QueueDailyReport(ctx context.Context, now time.Time) (PollResult, error) {
	m.initDelivery(now)
	outcome := PollResult{Success: true}
	if !m.Config.UsesCodexResets() || !m.Config.DailyReport.Enabled {
		return outcome, nil
	}
	date, due, err := dailyDate(now, m.Config.DailyReport)
	if err != nil || !due {
		return outcome, err
	}
	scheduled, err := dailyScheduledAt(now, m.Config.DailyReport)
	if err != nil || scheduled.IsZero() || scheduled.Before(m.deliverySince()) {
		return outcome, err
	}
	needed, err := m.Store.DailyReportNeeded(ctx, date)
	if err != nil || !needed {
		return outcome, err
	}
	if m.ResetProvider == nil {
		return outcome, errors.New("codex-resets provider is not configured")
	}
	report, err := BuildCodexReport(ctx, m.ResetProvider, now, m.Config.DailyReport.Timezone)
	if err != nil {
		return m.resetFailure(ctx, "daily_report", err)
	}
	body, err := feishu.FormatCodexReport(report)
	if err != nil {
		return outcome, err
	}
	outcome.Added, err = m.Store.QueueDailyReport(ctx, date, body, now)
	if err != nil {
		return outcome, err
	}
	if err := m.Store.Recover(ctx, "daily_report", "每日重置状态卡片已恢复生成。", now); err != nil {
		return outcome, err
	}
	m.Log.Info("daily_report_queued", "date", date, "timezone", report.Timezone, "destinations", outcome.Added)
	return outcome, nil
}

func (m *Monitor) dailyReports(ctx context.Context) error {
	failures := 0
	for ctx.Err() == nil {
		outcome, err := m.QueueDailyReport(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		delay := 30 * time.Second
		if outcome.Success {
			failures = 0
		} else {
			failures++
			delay = max(delay, Backoff(delay, m.Config.Delivery.RetryMax, failures), outcome.RetryAfter)
		}
		if !wait(ctx, delay) {
			break
		}
	}
	return ctx.Err()
}
