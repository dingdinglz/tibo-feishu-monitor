package store

import (
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/config"
)

func TestDebugBatchesAreIndependentBoundedAndRetryable(t *testing.T) {
	s, _ := openTest(t)
	p := post("10", epoch)
	count, err := s.QueueDebug(ctx, p, epoch, format)
	if err != nil || count != 2 || pending(t, s) != 4 {
		t.Fatal(count, err)
	}
	count, err = s.QueueDebug(ctx, p, epoch.Add(time.Minute), format)
	if err != nil || count != 0 || pending(t, s) != 4 {
		t.Fatal("pending test batches duplicated", count, err)
	}
	job, err := s.Claim(ctx, "a", epoch, time.Second)
	if err != nil || job == nil || job.Kind != "debug" || job.Text != "10 /1" {
		t.Fatal(job, err)
	}
	if err := s.Failed(ctx, *job, epoch.Add(time.Minute), "temporary", false); err != nil {
		t.Fatal(err)
	}
	retry, err := s.Claim(ctx, "a", epoch.Add(time.Minute), time.Second)
	if err != nil || retry == nil || retry.ID != job.ID || retry.Attempts != 2 {
		t.Fatal(retry, err)
	}
	if err := s.Sent(ctx, *retry, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, "a", epoch.Add(2*time.Minute), time.Second)
	if err != nil || second == nil || !strings.HasSuffix(second.Text, "/2") {
		t.Fatal(second, err)
	}
	if err := s.Sent(ctx, *second, epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	count, err = s.QueueDebug(ctx, p, epoch.Add(3*time.Minute), format)
	if err != nil || count != 1 || pending(t, s) != 4 {
		t.Fatal("healthy target should replay while failed target retains its batch", count, err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.Posts != 0 || st.Scan.Initialized {
		t.Fatal("debug changed normal state", st, err)
	}
}

func TestDebugDoesNotPoisonPostDedupOrPreemptNormalDeliveries(t *testing.T) {
	s, _ := openTest(t)
	p := post("10", epoch.Add(time.Minute))
	if _, err := s.QueueDebug(ctx, p, epoch, format); err != nil {
		t.Fatal(err)
	}
	apply(t, s, p)
	if pending(t, s) != 8 {
		t.Fatal("debug suppressed real notification")
	}
	job, err := s.Claim(ctx, "a", epoch.Add(2*time.Minute), time.Second)
	if err != nil || job == nil || job.Kind != "post" {
		t.Fatal("debug preempted real post", job, err)
	}
}

func TestDebugRespectsDisabledDestinations(t *testing.T) {
	s, _ := openTest(t)
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	count, err := s.QueueDebug(ctx, post("10", epoch), epoch, format)
	if err != nil || count != 1 || pending(t, s) != 2 {
		t.Fatal(count, err)
	}
}
