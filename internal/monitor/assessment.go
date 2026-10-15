package monitor

import (
	"context"
	"fmt"
	"strings"

	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/store"
)

// prepareCard runs in the delivery worker, outside scan transactions. This also
// covers pre-upgrade queued cards and debug posts outside the catch-up window.
// A service failure must not hide the post or fabricate a probability.
func (m *Monitor) prepareCard(ctx context.Context, job store.Job) (string, error) {
	p := feishu.CardPost(job.Text)
	if p == nil || m.Assessor == nil {
		return job.Text, nil
	}
	batch := "post:" + p.ID
	if job.Kind == "debug" {
		// QueueDebug uses debug:<nanosecond>:<post>:<destination>:0. All
		// destinations share one real judgment, but the next poll cannot reuse it.
		batch = strings.TrimSuffix(job.DedupKey, ":"+job.Destination+":0")
		if batch == "" {
			batch = fmt.Sprintf("debug-job:%d", job.ID)
		}
	}
	key := fmt.Sprintf("reset-v1:%s:%d:%d:%s", m.Config.TypeSafe.Model, m.Config.TypeSafe.RecentPosts, m.Config.TypeSafe.HistoryWindow, batch)
	m.assessmentMu.Lock()
	defer m.assessmentMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cached, err := m.Store.ResetAssessment(ctx, key)
	if err != nil {
		return "", err
	}
	if cached != nil {
		return feishu.WithResetAssessment(job.Text, *cached)
	}
	original, err := m.Store.Post(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if original != nil {
		p = original
	}
	history, err := m.Store.RecentPosts(ctx, *p, m.Config.TypeSafe.RecentPosts, m.Config.TypeSafe.HistoryWindow)
	if err != nil {
		return "", err
	}
	assessment, err := m.Assessor.Assess(ctx, *p, history)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || !assessment.Valid() {
		// Do not log arbitrary provider errors or substitute keyword-based scores.
		m.Log.Warn("reset_assessment_unavailable", "post_id", p.ID, "kind", job.Kind)
		return job.Text, nil
	}
	if err := m.Store.SaveResetAssessment(ctx, key, assessment); err != nil {
		return "", err
	}
	m.Log.Info("reset_assessed", "post_id", p.ID, "kind", job.Kind,
		"probability", *assessment.Probability, "model", assessment.Model, "history_posts", assessment.HistoryCount)
	return feishu.WithResetAssessment(job.Text, assessment)
}
