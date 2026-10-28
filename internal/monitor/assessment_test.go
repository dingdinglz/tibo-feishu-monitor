package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/store"
)

type recordingAssessor struct {
	calls   int
	post    model.Post
	history []model.Post
	err     error
}

func (a *recordingAssessor) Assess(_ context.Context, p model.Post, history []model.Post) (model.ResetAssessment, error) {
	a.calls++
	a.post, a.history = p, history
	value := 0.87
	return model.ResetAssessment{Probability: &value, Model: "jev-test", EvaluatedAt: time.Now(), HistoryCount: len(history)}, a.err
}

func requireAssessment(t *testing.T, body string) {
	t.Helper()
	var card struct {
		Reset *model.ResetAssessment `json:"reset_assessment"`
	}
	if err := json.Unmarshal([]byte(body), &card); err != nil || card.Reset == nil || !card.Reset.Valid() || *card.Reset.Probability != 0.87 {
		t.Fatal("card did not receive provider probability", body, err)
	}
}

func TestPrepareSharesRealJudgmentAcrossDestinationsAndRetries(t *testing.T) {
	m, f := newTest(t)
	a := &recordingAssessor{}
	m.Assessor = a
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	root := fixture("10", now.Add(-time.Minute))
	root.Text = "The reports of my reset button being lost are greatly exaggerated. Day 10/ Reset"
	root.Kind = "post"
	f.result = model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{root}}
	if _, err := m.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	if a.calls != 0 {
		t.Fatal("silent baseline spent an inference call")
	}
	p := fixture("11", now.Add(time.Minute))
	p.Text = "Landing within the next 2 hours"
	p.Parent = &model.Context{ID: root.ID, Author: root.Author, Text: strings.Repeat("full parent context ", 50), Available: true}
	f.result.Posts = []model.Post{p, root}
	if _, err := m.Poll(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var jobs []store.Job
	for _, dest := range []string{"a", "b"} {
		j, err := m.Store.Claim(ctx, dest, now.Add(4*time.Minute), time.Second)
		if err != nil || j == nil {
			t.Fatal(j, err)
		}
		jobs = append(jobs, *j)
	}
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	errs := make([]error, 2)
	for i := range jobs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); bodies[i], errs[i] = m.prepareCard(ctx, jobs[i]) }(i)
	}
	wg.Wait()
	for i := range bodies {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		requireAssessment(t, bodies[i])
	}
	if a.calls != 1 || len(a.history) != 1 || a.history[0].ID != root.ID || a.post.Parent.Text != p.Parent.Text {
		t.Fatal("did not use full evidence once", a)
	}
	// Reconstructing the monitor simulates losing all in-memory coalescing state.
	restarted := &Monitor{Config: m.Config, Store: m.Store, Assessor: a, Log: m.Log}
	body, err := restarted.prepareCard(ctx, jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	requireAssessment(t, body)
	if a.calls != 1 {
		t.Fatal("webhook retry repeated successful inference")
	}
}

func TestEveryIdleDebugBatchCallsAssessorAgainEvenForSameOldPost(t *testing.T) {
	m, f := newTest(t)
	m.Debug = true
	a := &recordingAssessor{}
	m.Assessor = a
	ctx := context.Background()
	now := time.Now().UTC()
	old := fixture("10", now.Add(-48*time.Hour))
	old.Text = "Done and dusted"
	old.Kind = "quote"
	old.Quote = &model.Context{ID: "9", Text: "Reset landing in 2 hours", Available: true}
	f.result = model.FetchResult{HeadOK: true, Complete: true, Latest: &old}
	for round := 0; round < 2; round++ {
		at := now.Add(time.Duration(round) * 3 * time.Minute)
		if _, err := m.Poll(ctx, at); err != nil {
			t.Fatal(err)
		}
		for _, dest := range []string{"a", "b"} {
			j, err := m.Store.Claim(ctx, dest, at, time.Second)
			if err != nil || j == nil || j.Kind != "debug" {
				t.Fatal(j, err)
			}
			text, err := m.prepareCard(ctx, *j)
			if err != nil {
				t.Fatal(err)
			}
			requireAssessment(t, text)
			if !strings.Contains(text, `"debug":true`) {
				t.Fatal("debug flag lost")
			}
			if err := m.Store.Sent(ctx, *j, at); err != nil {
				t.Fatal(err)
			}
		}
		if a.calls != round+1 {
			t.Fatal("debug reused prior batch or ran per destination", a.calls)
		}
	}
	st, err := m.Store.Status(ctx)
	if err != nil || st.Posts != 0 || st.Sent != 4 {
		t.Fatal(st, err)
	}
}

func TestAssessmentFailurePreservesCardAndDoesNotCacheFakeProbability(t *testing.T) {
	m, _ := newTest(t)
	a := &recordingAssessor{err: errors.New("DO_NOT_LEAK")}
	m.Assessor = a
	p := fixture("10", time.Now())
	bodies, err := feishu.Format(p)
	if err != nil {
		t.Fatal(err)
	}
	job := store.Job{ID: 1, Kind: "post", Text: bodies[0]}
	text, err := m.prepareCard(context.Background(), job)
	if err != nil || text != job.Text || strings.Contains(text, "DO_NOT_LEAK") {
		t.Fatal(text, err)
	}
	a.err = nil
	text, err = m.prepareCard(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	requireAssessment(t, text)
	if a.calls != 2 {
		t.Fatal("failure was cached as a probability")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.prepareCard(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation swallowed", err)
	}
	if a.calls != 2 {
		t.Fatal("canceled preparation called provider")
	}
	if text, err := m.prepareCard(context.Background(), store.Job{Kind: "alert", Text: "operational alert"}); err != nil || text != "operational alert" {
		t.Fatal("alert was analyzed", text, err)
	}
}

type captureSend struct {
	body   string
	cancel context.CancelFunc
}

func (s *captureSend) Send(_ context.Context, _ config.Destination, body string) error {
	s.body = body
	s.cancel()
	return nil
}

func TestDeliveryActuallyPreparesPostReplyAndQuoteBeforeSend(t *testing.T) {
	for _, kind := range []string{"post", "reply", "quote"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := newTest(t)
			a := &recordingAssessor{}
			m.Assessor = a
			now := time.Now().Add(-time.Hour)
			m.StartedAt = now.Add(-time.Second)
			p := fixture("10", now)
			p.Kind = kind
			if _, err := m.Store.ApplyScan(context.Background(), model.FetchResult{Posts: []model.Post{p}}, store.ScanState{Initialized: true, Baseline: now}, now, now, feishu.Format); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sender := &captureSend{cancel: cancel}
			m.Sender = sender
			if err := m.deliver(ctx, m.Config.Webhooks[0]); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			requireAssessment(t, sender.body)
			if a.calls != 1 || a.post.Kind != kind {
				t.Fatal("sender bypassed inference", a)
			}
		})
	}
}
