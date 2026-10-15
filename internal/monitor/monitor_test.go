package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/store"
)

type fakeFetch struct {
	result   model.FetchResult
	err      error
	requests []model.FetchRequest
}

func (f *fakeFetch) Fetch(_ context.Context, r model.FetchRequest) (model.FetchResult, error) {
	f.requests = append(f.requests, r)
	return f.result, f.err
}
func newTest(t *testing.T) (*Monitor, *fakeFetch) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := config.Defaults()
	c.Webhooks = []config.Destination{{ID: "a"}, {ID: "b"}}
	if err = s.SyncDestinations(context.Background(), c.Target.UserID, c.Webhooks); err != nil {
		t.Fatal(err)
	}
	f := &fakeFetch{}
	return &Monitor{Config: c, Store: s, Fetcher: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, f
}
func fixture(id string, at time.Time) model.Post {
	return model.Post{ID: id, AuthorID: "1953337039510003712", Author: "thsottiaux", CreatedAt: at, Kind: "reply", Text: "Hello", URL: "https://x.com/thsottiaux/status/" + id}
}

func TestFirstRunSilentThenNewAndDelayedOlderID(t *testing.T) {
	m, f := newTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	old := fixture("100", now.Add(-time.Minute))
	f.result = model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{old}}
	if _, err := m.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	st, _ := m.Store.Status(ctx)
	if st.Pending != 0 || !st.Scan.Initialized {
		t.Fatal(st)
	}
	fresh := fixture("300", now.Add(time.Minute))
	f.result.Posts = []model.Post{old, fresh}
	if _, err := m.Poll(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	delayed := fixture("200", now.Add(30*time.Second))
	f.result.Posts = []model.Post{old, fresh, delayed}
	if _, err := m.Poll(ctx, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, _ = m.Store.Status(ctx)
	if st.Pending != 4 {
		t.Fatalf("late-visible older ID lost or duplicated: %+v", st)
	}
}

func TestEmptyFailureDoesNotInitialize(t *testing.T) {
	m, f := newTest(t)
	f.err = errors.New("upstream secret must not be logged")
	out, err := m.Poll(context.Background(), time.Now())
	if err != nil || out.Success {
		t.Fatal(out, err)
	}
	st, _ := m.Store.State(context.Background())
	if st.Initialized {
		t.Fatal("failed fetch initialized baseline")
	}
}

func TestPartialPersistsAndResumesWithoutAdvancingComplete(t *testing.T) {
	m, f := newTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	f.result = model.FetchResult{HeadOK: true, Complete: true}
	if _, err := m.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	f.result = model.FetchResult{HeadOK: true, Posts: []model.Post{fixture("101", now.Add(time.Minute))}, Cursor: "continuation", Error: "fetch_timeout"}
	f.err = errors.New("timeout")
	if _, err := m.Poll(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, _ := m.Store.Status(ctx)
	if !st.Scan.LastComplete.Equal(now) || st.Pending != 2 {
		t.Fatal(st)
	}
	f.err = nil
	f.result = model.FetchResult{HeadOK: true, Complete: false}
	if _, err := m.Poll(ctx, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lastReq := f.requests[len(f.requests)-1]
	if lastReq.Cursor != "continuation" {
		t.Fatal("cursor lost")
	}
	if !lastReq.Since.Equal(f.requests[1].Since) {
		t.Fatal("scan window moved during continuation")
	}
}

type blockingFetch struct{}

func (blockingFetch) Fetch(ctx context.Context, _ model.FetchRequest) (model.FetchResult, error) {
	<-ctx.Done()
	return model.FetchResult{}, ctx.Err()
}

type independentSender struct{ delivered chan string }

func (s independentSender) Send(ctx context.Context, d config.Destination, _ string) error {
	if d.ID == "a" {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case s.delivered <- d.ID:
	default:
	}
	return nil
}

func TestSlowScrapeAndTargetDoNotBlockHealthyTarget(t *testing.T) {
	m, _ := newTest(t)
	now := time.Now().Add(-time.Hour)
	_, err := m.Store.ApplyScan(context.Background(), model.FetchResult{Posts: []model.Post{fixture("42", now)}}, store.ScanState{Initialized: true, Baseline: now}, now, now, func(model.Post) ([]string, error) { return []string{"test"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	m.Fetcher = blockingFetch{}
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
	case <-time.After(2 * time.Second):
		t.Fatal("healthy webhook blocked")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung")
	}
}

func TestJitterAndBackoff(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		j := Jitter()
		if j < time.Second || j > 30*time.Second || j%time.Second != 0 {
			t.Fatal(j)
		}
		seen[j] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter constant")
	}
	if Backoff(time.Second, 30*time.Minute, 1000000) != 30*time.Minute {
		t.Fatal("backoff did not cap")
	}
	if Backoff(time.Second, time.Minute, 3) != 4*time.Second {
		t.Fatal("not exponential")
	}
}
