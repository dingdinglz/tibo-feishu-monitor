package monitor

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/store"
)

type Fetcher interface {
	Fetch(context.Context, model.FetchRequest) (model.FetchResult, error)
}
type Sender interface {
	Send(context.Context, config.Destination, string) error
}

type Assessor interface {
	Assess(context.Context, model.Post, []model.Post) (model.ResetAssessment, error)
}

type Monitor struct {
	Config       config.Config
	Store        *store.Store
	Fetcher      Fetcher
	Sender       Sender
	Log          *slog.Logger
	Debug        bool
	Assessor     Assessor
	assessmentMu sync.Mutex // coalesce per-post/multi-destination inference before sending
}

type PollResult struct {
	Success    bool
	RetryAfter time.Duration
	Added      int
}

func Jitter() time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(30))
	if err != nil {
		return 15 * time.Second
	}
	return time.Duration(n.Int64()+1) * time.Second
}

func Backoff(initial, maximum time.Duration, failures int) time.Duration {
	d := initial
	for i := 1; i < failures && d < maximum; i++ {
		if d > maximum/2 {
			return maximum
		}
		d *= 2
	}
	if d > maximum {
		return maximum
	}
	return d
}

func (m *Monitor) fault(ctx context.Context, key, detail, exclude string, threshold int) error {
	m.Log.Warn("monitor_fault", "component", key, "reason", detail)
	return m.Store.Fault(ctx, key, detail, exclude, time.Now(), threshold, m.Config.Alerts.Cooldown)
}

func (m *Monitor) Poll(ctx context.Context, now time.Time) (PollResult, error) {
	var outcome PollResult
	c := m.Config
	state, err := m.Store.State(ctx)
	if err != nil {
		return outcome, err
	}
	cache, err := m.Store.Contexts(ctx)
	if err != nil {
		return outcome, err
	}
	cutoff := now.Add(-c.Poll.CatchUpWindow)
	since := cutoff
	cursor := state.Cursor
	if cursor != "" && !state.ScanStarted.Before(cutoff) {
		since = state.ScanSince
	} else if cursor != "" {
		cursor = ""
		if err := m.fault(ctx, "window", "续抓已超过补抓窗口，未覆盖区间不再自动补发。", "", 1); err != nil {
			return outcome, err
		}
	}
	last := state.LastComplete
	if last.IsZero() {
		last = state.Baseline
	}
	if state.Initialized && last.Before(cutoff) {
		if err := m.fault(ctx, "window", "距离上次完整采集已超过补抓窗口，窗口外可能有未抓取消息。", "", 1); err != nil {
			return outcome, err
		}
	}
	res, fetchErr := m.Fetcher.Fetch(ctx, model.FetchRequest{
		AccountsDB: c.Scrape.AccountsDB, UserID: c.Target.UserID, Username: c.Target.Username,
		Proxy: c.Scrape.Proxy, Since: since, Cursor: cursor, MaxPages: c.Scrape.MaxPages,
		TimeoutSeconds: c.Scrape.Timeout.Seconds(), WaitSeconds: c.Scrape.WaitTimeout.Seconds(),
		ContextLimit: c.Scrape.ContextLimit, CachedContexts: cache, IncludeLatest: m.Debug,
	})
	if ctx.Err() != nil {
		return outcome, ctx.Err()
	}
	if res.HeadOK {
		if !state.Initialized {
			state.Initialized = true
			state.Baseline = now
			m.Log.Info("baseline_established", "since", now)
		}
		state.LastRequestSuccess = now
		state.Cursor = res.Cursor
		if res.Cursor != "" {
			state.ScanSince = since
			if cursor == "" {
				state.ScanStarted = now
			}
		} else {
			state.ScanSince = time.Time{}
			state.ScanStarted = time.Time{}
		}
		if res.Complete && fetchErr == nil {
			state.LastComplete = now
		}
		outcome.Added, err = m.Store.ApplyScan(ctx, res, state, now, cutoff, feishu.Format)
		if err != nil {
			return outcome, err
		}
		if res.Complete && fetchErr == nil {
			if err := m.Store.Recover(ctx, "coverage", "最近窗口的采集已恢复完整覆盖（以 X 实际返回的数据为准）。", now); err != nil {
				return outcome, err
			}
		} else {
			if err := m.fault(ctx, "coverage", "采集窗口尚未覆盖完整；已取得的数据已保存，将继续补抓。", "", c.Alerts.FailureThreshold); err != nil {
				return outcome, err
			}
		}
	}
	if fetchErr != nil || res.Error != "" || !res.HeadOK {
		// Never include an arbitrary fetcher's error text: it may contain a URL/token.
		reason := "抓取失败，请检查 Python 依赖、网络、账号会话和服务状态。"
		if res.Error != "" {
			reason += " 错误类别：" + res.Error
		}
		if err := m.fault(ctx, "scrape", reason, "", c.Alerts.FailureThreshold); err != nil {
			return outcome, err
		}
		outcome.RetryAfter = time.Duration(res.RetryAfterSeconds) * time.Second
		if res.Error == "no_account" || res.Error == "no_available_account" || res.Error == "account_unavailable" {
			outcome.RetryAfter = max(outcome.RetryAfter, c.Delivery.RetryMax)
		}
		return outcome, nil
	}
	if err := m.Store.Recover(ctx, "scrape", "抓取已恢复。", now); err != nil {
		return outcome, err
	}
	outcome.Success = true
	if m.Debug && outcome.Added == 0 {
		if latest := latestPost(res); latest != nil {
			targets, err := m.Store.QueueDebug(ctx, *latest, now, feishu.FormatDebug)
			if err != nil {
				return outcome, err
			}
			m.Log.Info("debug_latest_queued", "post_id", latest.ID, "destinations", targets)
		} else {
			m.Log.Warn("debug_latest_unavailable")
		}
	}
	m.Log.Info("poll_finished", "pages", res.Pages, "new_posts", outcome.Added, "complete", res.Complete, "duration", time.Since(now).Round(time.Millisecond))
	return outcome, nil
}

// Compare timestamps and decimal IDs, not response order (which may contain
// conversation ancestors or pinned/out-of-order entries).
func latestPost(result model.FetchResult) *model.Post {
	latest := result.Latest
	for i := range result.Posts {
		p := &result.Posts[i]
		if latest == nil || p.CreatedAt.After(latest.CreatedAt) ||
			(p.CreatedAt.Equal(latest.CreatedAt) && (len(p.ID) > len(latest.ID) ||
				(len(p.ID) == len(latest.ID) && p.ID > latest.ID))) {
			latest = p
		}
	}
	return latest
}

func wait(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (m *Monitor) deliver(ctx context.Context, d config.Destination) error {
	for ctx.Err() == nil {
		job, err := m.Store.Claim(ctx, d.ID, time.Now(), m.Config.Delivery.SendInterval)
		if err != nil {
			return err
		}
		if job == nil {
			if !wait(ctx, 250*time.Millisecond) {
				break
			}
			continue
		}
		text, err := m.prepareCard(ctx, *job)
		if err != nil {
			return err
		}
		err = m.Sender.Send(ctx, d, text)
		if ctx.Err() != nil {
			break
		} // unacknowledged job remains pending
		now := time.Now()
		if err == nil {
			if err := m.Store.Sent(ctx, *job, now); err != nil {
				return err
			}
			if err := m.Store.Recover(ctx, "delivery:"+d.ID, "机器人 "+d.ID+" 的发送已恢复。", now); err != nil {
				return err
			}
			m.Log.Info("delivery_sent", "destination", d.ID, "job", job.ID, "attempt", job.Attempts)
			continue
		}
		failure := &feishu.SendError{Code: "transport_error"}
		var classified *feishu.SendError
		if errors.As(err, &classified) {
			failure = classified
		}
		delay := Backoff(m.Config.Delivery.RetryInitial, m.Config.Delivery.RetryMax, job.Attempts)
		delay = max(delay, failure.RetryAfter)
		if err := m.Store.Failed(ctx, *job, now.Add(delay), failure.Code, failure.Permanent); err != nil {
			return err
		}
		threshold := m.Config.Alerts.FailureThreshold
		if failure.Permanent {
			threshold = 1
		}
		// Do not turn failed alerts into a recursive storm of new alerts.
		if job.Kind != "alert" || failure.Permanent {
			if err := m.fault(ctx, "delivery:"+d.ID, "机器人 "+d.ID+" 发送失败，类别："+failure.Code, d.ID, threshold); err != nil {
				return err
			}
		} else {
			m.Log.Warn("alert_delivery_failed", "destination", d.ID, "code", failure.Code)
		}
	}
	return ctx.Err()
}

// Run schedules the next fetch only AFTER the previous one finishes. Delivery
// workers are independent, so one slow/failed webhook cannot block other targets.
func (m *Monitor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, len(m.Config.Webhooks))
	for _, d := range m.Config.Webhooks {
		if !d.IsEnabled() {
			continue
		}
		wg.Add(1)
		go func(d config.Destination) {
			defer wg.Done()
			if err := m.deliver(ctx, d); err != nil && ctx.Err() == nil {
				errs <- err
				cancel()
			}
		}(d)
	}
	defer wg.Wait()
	failures := 0
	var fatal error
	for ctx.Err() == nil {
		outcome, err := m.Poll(ctx, time.Now().UTC())
		if err != nil {
			fatal = err
			break
		}
		delay := m.Config.Poll.Interval
		if outcome.Success {
			failures = 0
		} else {
			failures++
			delay = max(delay, Backoff(m.Config.Poll.Interval, m.Config.Delivery.RetryMax, failures), outcome.RetryAfter)
		}
		delay += Jitter()
		m.Log.Info("next_poll", "wait", delay)
		if !wait(ctx, delay) {
			break
		}
	}
	cancel()
	select {
	case err := <-errs:
		return err
	default:
	}
	if fatal != nil && !errors.Is(fatal, context.Canceled) {
		return fatal
	}
	return nil
}
