package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

func apiWatch() model.CodexWatch {
	chance := 40
	return model.CodexWatch{
		Level: "elevated", ResetChancePercent: &chance, ForecastWindow: "within 24h",
		ObservedAt: epoch.Add(-48 * time.Hour), ExpiresAt: epoch.Add(24 * time.Hour),
		Text: "possible reset", Source: model.CodexSource{Type: "observed"},
	}
}

func watchFormat(w model.CodexWatch) (string, error) { return w.Text, nil }

func TestCodexWatchDurableDedupAndDestinationRetry(t *testing.T) {
	s, path := openTest(t)
	watch := apiWatch()
	if n, err := s.QueueCodexWatch(ctx, &watch, epoch, watchFormat); err != nil || n != 1 || pending(t, s) != 2 {
		t.Fatal("first-run active watch was lost", n, err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.Scan.Initialized || st.Resets != 0 || st.Posts != 0 {
		t.Fatal("watch changed reset/post history or baseline", st, err)
	}
	for _, dest := range []string{"a", "b"} {
		job, err := s.Claim(ctx, dest, epoch, time.Second)
		if err != nil || job == nil || job.Kind != "watch" || !strings.HasPrefix(job.DedupKey, "watch:") {
			t.Fatal(job, err)
		}
		if dest == "a" {
			err = s.Sent(ctx, *job, epoch)
		} else {
			err = s.Failed(ctx, *job, epoch.Add(time.Hour), "transport", false)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "new"}}); err != nil {
		t.Fatal(err)
	}
	changed := watch
	chance := 80
	changed.Level, changed.ResetChancePercent, changed.Text = "strong", &chance, "revised text"
	changed.ExpiresAt = changed.ExpiresAt.Add(time.Hour)
	changed.ObservedAt = changed.ObservedAt.In(time.FixedZone("offset", 8*60*60))
	if n, err := s.QueueCodexWatch(ctx, &changed, epoch.Add(time.Minute), watchFormat); err != nil || n != 0 || pending(t, s) != 1 {
		t.Fatal("restart/edit/timezone/new destination replayed the same watch", n, err)
	}
	if job, err := s.Claim(ctx, "b", epoch.Add(2*time.Hour), time.Second); err != nil || job != nil {
		t.Fatal("disabled destination still delivered", job, err)
	}
	if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "new"}}); err != nil {
		t.Fatal(err)
	}
	job, err := s.Claim(ctx, "b", epoch.Add(2*time.Hour), time.Second)
	if err != nil || job == nil || job.Attempts != 2 || job.Text != watch.Text {
		t.Fatal("retry lost its original snapshot", job, err)
	}
	if err := s.Sent(ctx, *job, epoch.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	changed.ObservedAt = changed.ObservedAt.Add(time.Minute)
	if n, err := s.QueueCodexWatch(ctx, &changed, epoch.Add(3*time.Hour), watchFormat); err != nil || n != 1 || pending(t, s) != 3 {
		t.Fatal("new observation was not sent to all enabled destinations", n, err)
	}
}

func TestCodexWatchSkipsAbsentAndExpired(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch *model.CodexWatch
	}{
		{"absent", nil},
		{"expired", func() *model.CodexWatch { w := apiWatch(); w.ExpiresAt = epoch.Add(-time.Second); return &w }()},
		{"expiry boundary", func() *model.CodexWatch { w := apiWatch(); w.ExpiresAt = epoch; return &w }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTest(t)
			n, err := s.QueueCodexWatch(ctx, tc.watch, epoch, func(model.CodexWatch) (string, error) {
				t.Fatal("ignored watch reached formatter")
				return "", nil
			})
			if err != nil || n != 0 || pending(t, s) != 0 {
				t.Fatal(n, err)
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM codex_watches").Scan(&count); err != nil || count != 0 {
				t.Fatal("ignored watch consumed dedup identity", count, err)
			}
		})
	}
}

func TestCodexWatchAtomicFailureAndSourceIdentity(t *testing.T) {
	s, _ := openTest(t)
	watch := apiWatch()
	if _, err := s.QueueCodexWatch(ctx, &watch, epoch, func(model.CodexWatch) (string, error) {
		return "", errors.New("format")
	}); err == nil {
		t.Fatal("expected format error")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_watch BEFORE INSERT ON outbox WHEN NEW.destination='b'
BEGIN SELECT RAISE(ABORT, 'test queue failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueCodexWatch(ctx, &watch, epoch, watchFormat); err == nil {
		t.Fatal("expected queue failure")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM codex_watches").Scan(&count); err != nil || count != 0 || pending(t, s) != 0 {
		t.Fatal("failed transaction retained a watch or partial fanout", count, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_watch"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueCodexWatch(ctx, &watch, epoch, watchFormat); err != nil || n != 1 {
		t.Fatal("failure consumed the watch", n, err)
	}
	watch.Source = model.CodexSource{Type: "x_post", Author: "thsottiaux", URL: "https://x.com/thsottiaux/status/2110951585983631505"}
	if n, err := s.QueueCodexWatch(ctx, &watch, epoch, watchFormat); err != nil || n != 1 || pending(t, s) != 4 {
		t.Fatal("distinct source was suppressed", n, err)
	}
	watch.Level = "regular" // Reset type is not a valid watch level.
	if _, err := s.QueueCodexWatch(ctx, &watch, epoch, watchFormat); err == nil || pending(t, s) != 4 {
		t.Fatal("invalid watch was accepted", err)
	}
}
