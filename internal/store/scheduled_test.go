package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

func scheduledFixture() model.CodexScheduledReset {
	return model.CodexScheduledReset{ID: "announcement", Status: "scheduled", ResetType: "regular", AnnouncedAt: epoch, Text: "planned", Source: model.CodexSource{Type: "observed"}}
}

func scheduledFormat(r model.CodexScheduledReset) (string, error) { return r.Text, nil }

func TestScheduledResetDurableIdentityAndExecution(t *testing.T) {
	s, path := openTest(t)
	r := scheduledFixture()
	if n, err := s.QueueCodexScheduledReset(ctx, &r, scheduledFormat); err != nil || n != 1 || pending(t, s) != 2 {
		t.Fatal(n, err)
	}
	job, err := s.Claim(ctx, "a", epoch, time.Second)
	if err != nil || job == nil || job.Kind != "scheduled" || !strings.HasPrefix(job.DedupKey, "scheduled:") {
		t.Fatal(job, err)
	}
	if err := s.Sent(ctx, *job, epoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "new"}}); err != nil {
		t.Fatal(err)
	}
	r.Text = "revised"
	past := epoch.Add(-time.Hour)
	r.ScheduledFor = &past
	for _, v := range []*model.CodexScheduledReset{nil, &r, &r} {
		if n, err := s.QueueCodexScheduledReset(ctx, v, scheduledFormat); err != nil || n != 0 {
			t.Fatal(n, err)
		}
	}
	job, err = s.Claim(ctx, "b", epoch.Add(time.Minute), time.Second)
	if err != nil || job == nil || job.Text != "planned" {
		t.Fatal(job, err)
	}
	if n, err := s.ApplyCodexScan(ctx, []model.CodexReset{apiReset(r.ID, epoch.Add(time.Second))}, state(), epoch.Add(time.Minute), epoch, resetFormat); err != nil || n != 1 || pending(t, s) != 4 {
		t.Fatal("schedule suppressed execution or replayed to new destination", n, err)
	}
}

func TestScheduledResetAtomicValidation(t *testing.T) {
	s, _ := openTest(t)
	r := scheduledFixture()
	if _, err := s.QueueCodexScheduledReset(ctx, &r, func(model.CodexScheduledReset) (string, error) { return "", errors.New("format") }); err == nil {
		t.Fatal("expected formatting failure")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_scheduled BEFORE INSERT ON outbox WHEN NEW.destination='b' BEGIN SELECT RAISE(ABORT, 'queue failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueCodexScheduledReset(ctx, &r, scheduledFormat); err == nil {
		t.Fatal("expected queue failure")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM codex_scheduled_resets").Scan(&count); err != nil || count != 0 || pending(t, s) != 0 {
		t.Fatal(count, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_scheduled"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueCodexScheduledReset(ctx, &r, scheduledFormat); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	r.Status = "executed"
	if _, err := s.QueueCodexScheduledReset(ctx, &r, scheduledFormat); err == nil {
		t.Fatal("invalid schedule accepted")
	}
}
