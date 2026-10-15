package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func TestDebugReplaysLatestOnFirstAndSubsequentIdlePolls(t *testing.T) {
	m, f := newTest(t)
	m.Debug = true
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	latest := fixture("10", now.Add(-time.Minute))
	// Response order is not chronological; equal timestamps use numeric IDs.
	f.result = model.FetchResult{HeadOK: true, Complete: true, Posts: []model.Post{
		fixture("100", now.Add(-2*time.Minute)), latest, fixture("9", latest.CreatedAt),
	}}
	for round := 0; round < 2; round++ {
		at := now.Add(time.Duration(round) * 3 * time.Minute)
		out, err := m.Poll(ctx, at)
		if err != nil || !out.Success || out.Added != 0 {
			t.Fatal(out, err)
		}
		if !f.requests[round].IncludeLatest {
			t.Fatal("debug latest was not requested")
		}
		for _, dest := range []string{"a", "b"} {
			job, err := m.Store.Claim(ctx, dest, at, time.Second)
			if err != nil || job == nil {
				t.Fatal(job, err)
			}
			var card struct {
				Post  model.Post `json:"post"`
				Debug bool       `json:"debug"`
			}
			if err := json.Unmarshal([]byte(job.Text), &card); err != nil {
				t.Fatal(err)
			}
			if job.Kind != "debug" || !card.Debug || card.Post.ID != "10" {
				t.Fatalf("wrong fallback: %+v", job)
			}
			if err := m.Store.Sent(ctx, *job, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	state, _ := m.Store.State(ctx)
	if !state.Baseline.Equal(now) {
		t.Fatal("debug changed baseline")
	}
}

func TestDebugDoesNotAddFallbackAlongsideNewPosts(t *testing.T) {
	m, f := newTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	f.result = model.FetchResult{HeadOK: true, Complete: true}
	if _, err := m.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	m.Debug = true
	fresh := fixture("42", now.Add(time.Minute))
	f.result.Posts = []model.Post{fresh}
	f.result.Latest = &fresh
	out, err := m.Poll(ctx, now.Add(3*time.Minute))
	if err != nil || out.Added != 1 {
		t.Fatal(out, err)
	}
	st, _ := m.Store.Status(ctx)
	if st.Pending != 2 {
		t.Fatalf("extra fallback alongside new post: %+v", st)
	}
	job, err := m.Store.Claim(ctx, "a", now.Add(3*time.Minute), time.Second)
	if err != nil || job == nil || job.Kind != "post" || strings.Contains(job.Text, "DEBUG") {
		t.Fatal(job, err)
	}
}

func TestDebugCanSendLatestOutsideCatchupWithoutChangingNormalPosts(t *testing.T) {
	m, f := newTest(t)
	m.Debug = true
	ctx := context.Background()
	now := time.Now().UTC()
	old := fixture("11", now.Add(-48*time.Hour))
	f.result = model.FetchResult{HeadOK: true, Complete: true, Latest: &old}
	if out, err := m.Poll(ctx, now); err != nil || out.Added != 0 {
		t.Fatal(out, err)
	}
	st, _ := m.Store.Status(ctx)
	if st.Pending != 2 || st.Posts != 0 {
		t.Fatalf("debug polluted normal discoveries: %+v", st)
	}
}

func TestDebugSkipsFailuresMissingDataAndDisabledMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		debug, head bool
		failure     error
		errorCode   string
		latest      bool
	}{
		{"off", false, true, nil, "", true},
		{"fetch_error", true, true, errors.New("failure"), "timeout", true},
		{"business_error", true, true, nil, "no_account", true},
		{"invalid_head", true, false, nil, "", true},
		{"no_latest", true, true, nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newTest(t)
			m.Debug = tc.debug
			f.result = model.FetchResult{HeadOK: tc.head, Complete: tc.errorCode == "", Error: tc.errorCode}
			if tc.latest {
				p := fixture("11", time.Now().Add(-time.Hour))
				f.result.Latest = &p
			}
			f.err = tc.failure
			if _, err := m.Poll(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			st, _ := m.Store.Status(context.Background())
			if st.Pending != 0 {
				t.Fatalf("unexpected debug message: %+v", st)
			}
			if f.requests[0].IncludeLatest != tc.debug {
				t.Fatal("wrong include_latest setting")
			}
		})
	}
}
