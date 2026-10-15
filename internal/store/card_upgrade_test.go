package store

import (
	"errors"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func singleCard(p model.Post) ([]string, error) { return []string{"card:" + p.ID}, nil }

func TestUpgradePendingCardsCollapsesLegacyFragmentsAndPreservesSent(t *testing.T) {
	s, _ := openTest(t)
	apply(t, s, post("10", epoch)) // legacy formatter creates two fragments per target
	first, err := s.Claim(ctx, "a", epoch.Add(time.Minute), time.Second)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	if err = s.Sent(ctx, *first, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pendingJob, err := s.Claim(ctx, "a", epoch.Add(2*time.Minute), time.Second)
	if err != nil || pendingJob == nil {
		t.Fatal(pendingJob, err)
	}
	if err = s.Failed(ctx, *pendingJob, epoch.Add(time.Hour), "temporary", false); err != nil {
		t.Fatal(err)
	}
	count, err := s.UpgradePendingPosts(ctx, singleCard)
	if err != nil || count != 2 {
		t.Fatal(count, err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.Sent != 1 || st.Pending != 2 || st.Posts != 1 {
		t.Fatal(st, err)
	}
	if count, err = s.UpgradePendingPosts(ctx, singleCard); err != nil || count != 0 {
		t.Fatal("upgrade is not idempotent", count, err)
	}
	if job, err := s.Claim(ctx, "a", epoch.Add(3*time.Minute), time.Second); err != nil || job != nil {
		t.Fatal("retry cooldown lost", job, err)
	}
	job, err := s.Claim(ctx, "a", epoch.Add(2*time.Hour), time.Second)
	if err != nil || job == nil || job.ID != pendingJob.ID || job.Attempts != 2 || job.Text != "card:10" {
		t.Fatal("pending task identity changed", job, err)
	}
	other, err := s.Claim(ctx, "b", epoch.Add(2*time.Hour), time.Second)
	if err != nil || other == nil || other.Text != "card:10" {
		t.Fatal(other, err)
	}
}

func TestUpgradeCardsIsAtomicAndRejectsMultipleOutputs(t *testing.T) {
	for _, formatter := range []Formatter{
		func(model.Post) ([]string, error) { return nil, errors.New("format failure") },
		format,
	} {
		s, _ := openTest(t)
		apply(t, s, post("10", epoch), post("20", epoch.Add(time.Second)))
		if _, err := s.UpgradePendingPosts(ctx, formatter); err == nil {
			t.Fatal("expected upgrade failure")
		}
		if pending(t, s) != 8 {
			t.Fatal("failed upgrade changed queue")
		}
	}
}
