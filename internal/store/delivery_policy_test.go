package store

import (
	"reflect"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

// Keep the complete delivery snapshot so filtering cannot silently delete,
// supersede, acknowledge, or increment attempts on an ineligible task.
type deliveryRow struct {
	ID, CreatedAt, NextAttempt, SentAt int64
	Destination, Key, Kind, Body       string
	Status, LastError                  string
	Attempts                           int
}

func deliveryRows(t *testing.T, s *Store) map[int64]deliveryRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT id,created_at,next_attempt,sent_at,destination,dedup_key,kind,body,status,last_error,attempts FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := make(map[int64]deliveryRow)
	for rows.Next() {
		var r deliveryRow
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.NextAttempt, &r.SentAt, &r.Destination, &r.Key, &r.Kind, &r.Body, &r.Status, &r.LastError, &r.Attempts); err != nil {
			t.Fatal(err)
		}
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func deliveryReopen(t *testing.T, s *Store, path string, policy DeliveryPolicy) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	next.SetDeliveryPolicy(policy)
	return next
}

func deliveryNoClaim(t *testing.T, s *Store, dest string) {
	t.Helper()
	if job, err := s.Claim(ctx, dest, epoch.Add(72*time.Hour), 0); err != nil || job != nil {
		t.Fatalf("Claim(%s) = %+v, %v; want no eligible task", dest, job, err)
	}
}

func deliveryDrain(t *testing.T, s *Store, dest string) []Job {
	t.Helper()
	var jobs []Job
	for i := 0; i < 100; i++ {
		job, err := s.Claim(ctx, dest, epoch.Add(72*time.Hour), 0)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			return jobs
		}
		jobs = append(jobs, *job)
		if err := s.Sent(ctx, *job, epoch.Add(72*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("queue did not drain within fixture size")
	return nil
}

// Both event scanners use the same cases, including two-part post fanout.
func deliveryScan(t *testing.T, s *Store, kind string, events []model.Post, baseline, cutoff time.Time) int {
	t.Helper()
	st := ScanState{Initialized: true, Baseline: baseline, LastComplete: epoch.Add(24 * time.Hour)}
	var n int
	var err error
	if kind == "post" {
		n, err = s.ApplyScan(ctx, model.FetchResult{Posts: events}, st, epoch.Add(24*time.Hour), cutoff, format)
	} else {
		resets := make([]model.CodexReset, len(events))
		for i, p := range events {
			resets[i] = apiReset(p.ID, p.CreatedAt)
			resets[i].Text = p.Text
		}
		n, err = s.ApplyCodexScan(ctx, resets, st, epoch.Add(24*time.Hour), cutoff,
			func(r model.CodexReset, _ bool) (string, error) { return r.Text, nil })
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func deliverySeedAll(t *testing.T, s *Store, at time.Time) {
	t.Helper()
	for _, kind := range []string{"post", "reset"} {
		if n := deliveryScan(t, s, kind, []model.Post{post("shared", at)}, epoch.Add(-time.Hour), epoch.Add(-time.Hour)); n != 1 {
			t.Fatalf("seed %s = %d", kind, n)
		}
	}
	w := apiWatch()
	w.ObservedAt, w.ExpiresAt = at, epoch.Add(96*time.Hour)
	if n, err := s.QueueCodexWatch(ctx, &w, epoch, watchFormat); err != nil || n != 1 {
		t.Fatal("seed watch", n, err)
	}
	r := scheduledFixture()
	r.ID, r.AnnouncedAt = "shared", at
	future := epoch.Add(96 * time.Hour)
	r.ScheduledFor = &future
	if n, err := s.QueueCodexScheduledReset(ctx, &r, scheduledFormat); err != nil || n != 1 {
		t.Fatal("seed scheduled", n, err)
	}
	if err := s.Fault(ctx, "old-alert", "old failure", "", at, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueDailyReport(ctx, "2026-01-01", "old daily", at); err != nil || n != 2 {
		t.Fatal("seed daily", n, err)
	}
	if n, err := s.QueueCodexDebug(ctx, apiReset("debug", at), at, resetFormat); err != nil || n != 2 {
		t.Fatal("seed debug", n, err)
	}
	if got := pending(t, s); got != 16 {
		t.Fatalf("seed pending = %d, want 16", got)
	}
}

func TestDeliveryPolicyRestartRetainsEveryOldKindAndDoesNotBlockNewEvents(t *testing.T) {
	s, path := openTest(t)
	deliverySeedAll(t, s, epoch)
	before := deliveryRows(t, s)
	start := epoch.Add(time.Hour)
	s = deliveryReopen(t, s, path, DeliveryPolicy{Since: start})
	for _, dest := range []string{"a", "b"} {
		deliveryNoClaim(t, s, dest)
	}
	if after := deliveryRows(t, s); !reflect.DeepEqual(after, before) || pending(t, s) != 16 {
		t.Fatalf("startup filtering changed old pending tasks: before=%+v after=%+v", before, after)
	}
	for _, kind := range []string{"post", "reset"} {
		if n := deliveryScan(t, s, kind, []model.Post{post("fresh", start)}, epoch, epoch); n != 1 {
			t.Fatalf("fresh %s added = %d", kind, n)
		}
	}
	for _, dest := range []string{"a", "b"} {
		jobs := deliveryDrain(t, s, dest)
		counts := map[string]int{}
		for _, j := range jobs {
			counts[j.Kind]++
			if _, old := before[j.ID]; old {
				t.Fatalf("old task escaped boundary: %+v", j)
			}
		}
		if !reflect.DeepEqual(counts, map[string]int{"post": 2, "reset": 1}) {
			t.Fatalf("old alert blocked fresh events for %s: %+v", dest, jobs)
		}
	}
	after := deliveryRows(t, s)
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("old task %d changed: got %+v, want %+v", id, got, want)
		}
	}
}

func TestDeliveryPolicyExplicitSinceReleasesOnlyRangeAndNeverSent(t *testing.T) {
	s, path := openTest(t)
	at := epoch.Add(time.Minute)
	deliverySeedAll(t, s, at)
	deliveryScan(t, s, "post", []model.Post{post("outside-range", epoch)}, epoch, epoch)
	// Alert priority gives us a real acknowledged task before closing the store.
	job, err := s.Claim(ctx, "a", epoch.Add(2*time.Hour), 0)
	if err != nil || job == nil || job.Kind != "alert" {
		t.Fatal(job, err)
	}
	if err := s.Sent(ctx, *job, epoch.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	before := deliveryRows(t, s)
	s = deliveryReopen(t, s, path, DeliveryPolicy{Since: epoch.Add(3 * time.Hour)})
	deliveryNoClaim(t, s, "a")
	deliveryNoClaim(t, s, "b")
	s = deliveryReopen(t, s, path, DeliveryPolicy{Since: at, Replay: true})
	claimed := map[int64]bool{}
	for _, dest := range []string{"a", "b"} {
		for _, j := range deliveryDrain(t, s, dest) {
			if claimed[j.ID] {
				t.Fatalf("task delivered twice: %+v", j)
			}
			claimed[j.ID] = true
		}
	}
	after := deliveryRows(t, s)
	if len(after) != len(before) {
		t.Fatal("changing policy changed queue size")
	}
	for id, old := range before {
		eligible := old.Status == "pending" && old.CreatedAt >= at.UnixMilli()
		if claimed[id] != eligible {
			t.Fatalf("task %+v claimed=%v, want %v", old, claimed[id], eligible)
		}
		if !eligible && after[id] != old {
			t.Fatalf("sent/out-of-range task was changed: %+v -> %+v", old, after[id])
		}
	}
	if len(claimed) != 15 || pending(t, s) != 4 {
		t.Fatalf("claimed=%d pending=%d; want 15 and 4", len(claimed), pending(t, s))
	}
	s = deliveryReopen(t, s, path, DeliveryPolicy{Since: at, Replay: true})
	deliveryNoClaim(t, s, "a")
	deliveryNoClaim(t, s, "b")
}

func TestDeliveryPolicyDebugBypassIsKindScopedAndZeroPolicyStaysUnrestricted(t *testing.T) {
	s, path := openTest(t)
	deliverySeedAll(t, s, epoch)
	before := deliveryRows(t, s)
	s = deliveryReopen(t, s, path, DeliveryPolicy{Since: epoch.Add(time.Hour), Debug: true})
	for _, dest := range []string{"a", "b"} {
		jobs := deliveryDrain(t, s, dest)
		if len(jobs) != 1 || jobs[0].Kind != "debug" {
			t.Fatalf("debug bypass released ordinary backlog for %s: %+v", dest, jobs)
		}
	}
	after := deliveryRows(t, s)
	for id, old := range before {
		if old.Kind != "debug" && after[id] != old {
			t.Fatalf("debug bypass modified ordinary task: %+v -> %+v", old, after[id])
		}
	}
	// Debug must not bypass event-time filtering at discovery either.
	for _, kind := range []string{"post", "reset"} {
		if n := deliveryScan(t, s, kind, []model.Post{post("old-discovery", epoch)}, epoch, epoch); n != 0 {
			t.Fatalf("debug bypass queued an old %s", kind)
		}
	}
	if got := pending(t, s); got != 14 {
		t.Fatalf("ordinary pending = %d, want 14", got)
	}
	s = deliveryReopen(t, s, path, DeliveryPolicy{})
	for _, dest := range []string{"a", "b"} {
		if jobs := deliveryDrain(t, s, dest); len(jobs) != 7 {
			t.Fatalf("zero policy restricted low-level queue: %+v", jobs)
		}
	}
}

func TestDeliveryPolicyReplayDiscoveredEventsAndNewDestinationsWithoutDuplicates(t *testing.T) {
	for _, kind := range []string{"post", "reset"} {
		t.Run(kind, func(t *testing.T) {
			s, path := openTest(t)
			start := epoch.Add(time.Hour)
			events := []model.Post{post("too-old", epoch.Add(-time.Nanosecond)), post("filtered", epoch.Add(time.Minute)), post("queued", start)}
			s.SetDeliveryPolicy(DeliveryPolicy{Since: start})
			if n := deliveryScan(t, s, kind, events, epoch, epoch); n != 1 {
				t.Fatalf("default scan added=%d, want 1", n)
			}
			st, err := s.Status(ctx)
			if err != nil || (kind == "post" && st.Posts != 3) || (kind == "reset" && st.Resets != 3) {
				t.Fatal("filtered discoveries were not persisted", st, err)
			}
			job, err := s.Claim(ctx, "a", start, 0)
			if err != nil || job == nil {
				t.Fatal(job, err)
			}
			if err := s.Sent(ctx, *job, start); err != nil {
				t.Fatal(err)
			}
			before := deliveryRows(t, s)
			s = deliveryReopen(t, s, path, DeliveryPolicy{Since: epoch})
			if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
				t.Fatal(err)
			}
			if n := deliveryScan(t, s, kind, events, epoch, epoch); n != 0 {
				t.Fatalf("without Replay, rediscovery added=%d", n)
			}
			if after := deliveryRows(t, s); !reflect.DeepEqual(before, after) {
				t.Fatal("without Replay, old discoveries or new destinations received history")
			}
			s = deliveryReopen(t, s, path, DeliveryPolicy{Since: epoch, Replay: true})
			for i := range events {
				events[i].Text = "reformatted " + events[i].ID
			}
			for i, want := range []int{2, 0, 0} {
				if n := deliveryScan(t, s, kind, events, epoch, epoch); n != want {
					t.Fatalf("replay scan %d added=%d, want %d", i, n, want)
				}
			}
			after := deliveryRows(t, s)
			parts := 1
			if kind == "post" {
				parts = 2
			}
			if len(after) != 2*3*parts {
				t.Fatalf("replay row count=%d, want %d", len(after), 2*3*parts)
			}
			for id, old := range before {
				if after[id] != old {
					t.Fatalf("replay changed pending/sent snapshot: %+v -> %+v", old, after[id])
				}
			}
			for _, dest := range []string{"a", "b", "c"} {
				want := 2 * parts
				if dest == "a" {
					want-- // The first fragment/reset was already acknowledged.
				}
				jobs := deliveryDrain(t, s, dest)
				if len(jobs) != want {
					t.Fatalf("%s replay deliveries=%d, want %d", dest, len(jobs), want)
				}
				for _, j := range jobs {
					if j.ID == job.ID {
						t.Fatal("replay resent an acknowledged task")
					}
				}
			}
			if n := deliveryScan(t, s, kind, events, epoch, epoch); n != 0 || pending(t, s) != 0 {
				t.Fatal("replay recreated sent deliveries", n)
			}
		})
	}
}

func TestDeliveryPolicyScannersUseEventTimeAndAllInclusiveBounds(t *testing.T) {
	for _, kind := range []string{"post", "reset"} {
		for _, gate := range []string{"since", "baseline", "cutoff"} {
			t.Run(kind+"/"+gate, func(t *testing.T) {
				s, _ := openTest(t)
				boundary := epoch.Add(time.Hour + 123*time.Nanosecond)
				since, baseline, cutoff := epoch, epoch, epoch
				switch gate {
				case "since":
					since = boundary
				case "baseline":
					baseline = boundary
				case "cutoff":
					cutoff = boundary
				}
				s.SetDeliveryPolicy(DeliveryPolicy{Since: since, Replay: true})
				events := []model.Post{post("before", boundary.Add(-time.Nanosecond)), post("equal", boundary), post("after", boundary.Add(time.Nanosecond))}
				if n := deliveryScan(t, s, kind, events, baseline, cutoff); n != 2 {
					t.Fatalf("scan added=%d, want exactly equal and after", n)
				}
				if n := deliveryScan(t, s, kind, events, baseline, cutoff); n != 0 {
					t.Fatalf("repeat scan added=%d", n)
				}
				counts := map[string]int{}
				for _, r := range deliveryRows(t, s) {
					counts[r.Body]++
				}
				want := map[string]int{"equal": 2, "after": 2}
				if kind == "post" {
					want = map[string]int{"equal /1": 2, "equal /2": 2, "after /1": 2, "after /2": 2}
				}
				if !reflect.DeepEqual(counts, want) {
					t.Fatalf("event-time filtering got=%v, want=%v", counts, want)
				}
			})
		}
	}
}

func TestDeliveryPolicyClaimMillisecondBoundaryIsConservative(t *testing.T) {
	for _, tc := range []struct {
		name      string
		since, at time.Duration
		claim     bool
	}{
		{"aligned_before", time.Second, time.Second - time.Nanosecond, false},
		{"aligned_equal", time.Second, time.Second, true},
		{"aligned_after", time.Second, time.Second + time.Nanosecond, true},
		{"fraction_before", time.Second + time.Nanosecond, time.Second, false},
		{"fraction_equal_ambiguous", time.Second + time.Nanosecond, time.Second + time.Nanosecond, false},
		{"fraction_after_ambiguous", time.Second + time.Nanosecond, time.Second + time.Millisecond - time.Nanosecond, false},
		{"fraction_next_millisecond", time.Second + time.Nanosecond, time.Second + time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := openTest(t)
			deliverySeedAll(t, s, epoch.Add(tc.at))
			before := deliveryRows(t, s)
			s = deliveryReopen(t, s, path, DeliveryPolicy{Since: epoch.Add(tc.since)})
			for _, dest := range []string{"a", "b"} {
				jobs := deliveryDrain(t, s, dest)
				want := 0
				if tc.claim {
					want = 8
				}
				if len(jobs) != want {
					t.Fatalf("claimed %d, want %d at event=%s boundary=%s", len(jobs), want, epoch.Add(tc.at), epoch.Add(tc.since))
				}
			}
			if !tc.claim && !reflect.DeepEqual(deliveryRows(t, s), before) {
				t.Fatal("millisecond filtering mutated ambiguous pending tasks")
			}
		})
	}
}

func TestDeliveryPolicyWatchAndScheduledUseAnnouncementTimeWithoutConsumingDedup(t *testing.T) {
	for _, kind := range []string{"watch", "scheduled"} {
		t.Run(kind, func(t *testing.T) {
			s, path := openTest(t)
			boundary := epoch.Add(time.Hour + 123*time.Nanosecond)
			at := boundary.Add(-time.Nanosecond)
			w := apiWatch()
			w.ObservedAt, w.ExpiresAt = at, epoch.Add(96*time.Hour)
			r := scheduledFixture()
			r.ID, r.AnnouncedAt = "shared", at
			future := epoch.Add(96 * time.Hour)
			r.ScheduledFor = &future
			calls := 0
			queue := func() int {
				t.Helper()
				var n int
				var err error
				if kind == "watch" {
					n, err = s.QueueCodexWatch(ctx, &w, epoch.Add(2*time.Hour), func(w model.CodexWatch) (string, error) { calls++; return w.Text, nil })
				} else {
					n, err = s.QueueCodexScheduledReset(ctx, &r, func(r model.CodexScheduledReset) (string, error) { calls++; return r.Text, nil })
				}
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			s.SetDeliveryPolicy(DeliveryPolicy{Since: boundary, Debug: true})
			if n := queue(); n != 0 || calls != 0 || pending(t, s) != 0 {
				t.Fatalf("future expiry/execution or Debug rescued old announcement: n=%d calls=%d", n, calls)
			}
			table := "codex_watches"
			if kind == "scheduled" {
				table = "codex_scheduled_resets"
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatal("filter consumed dedup identity", count, err)
			}
			// No Replay is needed: the filtered observation/announcement was not consumed.
			s = deliveryReopen(t, s, path, DeliveryPolicy{Since: at})
			if n := queue(); n != 1 || pending(t, s) != 2 {
				t.Fatal("exact boundary did not queue filtered announcement", n)
			}
			before := deliveryRows(t, s)
			if err := s.SyncDestinations(ctx, "target", []config.Destination{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
				t.Fatal(err)
			}
			if n := queue(); n != 0 || !reflect.DeepEqual(deliveryRows(t, s), before) {
				t.Fatal("without Replay a new destination received history", n)
			}
			// A confirmed execution is independent of the announcement/watch namespace.
			if n := deliveryScan(t, s, "reset", []model.Post{post("shared", boundary.Add(time.Millisecond))}, epoch, epoch); n != 1 {
				t.Fatal("announcement suppressed confirmed reset", n)
			}
			s = deliveryReopen(t, s, path, DeliveryPolicy{Since: at, Replay: true})
			for _, want := range []int{1, 0, 0} {
				if n := queue(); n != want {
					t.Fatalf("announcement replay added=%d, want %d", n, want)
				}
			}
			if got := pending(t, s); got != 6 {
				t.Fatalf("announcement plus execution fanout=%d, want 6", got)
			}
			for id, old := range before {
				if deliveryRows(t, s)[id] != old {
					t.Fatal("announcement replay changed an existing task")
				}
			}
		})
	}
}

func TestDeliveryPolicyWatchExpiryStillAppliesWithReplay(t *testing.T) {
	for _, expiry := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(expiry.String(), func(t *testing.T) {
			s, _ := openTest(t)
			s.SetDeliveryPolicy(DeliveryPolicy{Since: epoch, Replay: true})
			w := apiWatch()
			w.ObservedAt, w.ExpiresAt = epoch, epoch.Add(time.Hour).Add(expiry)
			calls := 0
			n, err := s.QueueCodexWatch(ctx, &w, epoch.Add(time.Hour), func(w model.CodexWatch) (string, error) { calls++; return w.Text, nil })
			want := 0
			if expiry > 0 {
				want = 1
			}
			var count int
			if e := s.db.QueryRow("SELECT count(*) FROM codex_watches").Scan(&count); e != nil {
				t.Fatal(e)
			}
			if err != nil || n != want || calls != want || count != want || pending(t, s) != 2*want {
				t.Fatalf("expiry: n=%d calls=%d dedup=%d pending=%d err=%v; want event count %d", n, calls, count, pending(t, s), err, want)
			}
		})
	}
}
