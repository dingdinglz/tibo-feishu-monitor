package codexresets

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestCooldownSharedByPollingAndDailyRequests(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 59, 0, 0, time.UTC)
	var calls atomic.Int32
	c := testClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/api/v1/status" {
			_, _ = io.WriteString(w, sampleStatus)
		} else {
			_, _ = io.WriteString(w, pageJSON("[]", false, "null"))
		}
	})
	c.now = func() time.Time { return now }
	_, err := c.Resets(context.Background(), time.Time{})
	clientError(t, err, "http_429")
	// The daily status call at 09:00 Shanghai must not bypass the poll's 429.
	now = now.Add(time.Minute)
	_, err = c.Status(context.Background())
	if got := clientError(t, err, "http_429"); got.RetryAfter != 59*time.Minute || calls.Load() != 1 {
		t.Fatal("another consumer bypassed the server cooldown", got, calls.Load())
	}
	_, err = c.Resets(context.Background(), time.Time{})
	clientError(t, err, "http_429")
	if calls.Load() != 1 {
		t.Fatal("cooling history still reached upstream")
	}
	now = now.Add(59 * time.Minute)
	if _, err := c.Status(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatal("expired cooldown prevented request", err)
	}
}

func TestSharedCooldownKeepsLongestDeadline(t *testing.T) {
	c := New(time.Second)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.rememberCooldown(&Error{Code: "http_429", RetryAfter: time.Hour})
	c.rememberCooldown(&Error{Code: "http_503", RetryAfter: time.Minute})
	if got := clientError(t, c.cooldown(), "http_429"); got.RetryAfter != time.Hour {
		t.Fatal("shorter concurrent hint shortened cooldown")
	}
	now = now.Add(2 * time.Hour)
	if err := c.cooldown(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalRequestsRemainConcurrent(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := testClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			close(started)
			<-release
			_, _ = io.WriteString(w, sampleStatus)
		} else {
			_, _ = io.WriteString(w, pageJSON("[]", false, "null"))
		}
	})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := c.Status(context.Background())
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("status did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := c.Resets(ctx, time.Time{}); err != nil {
		t.Fatal("status request serialized an independent history request", err)
	}
}
