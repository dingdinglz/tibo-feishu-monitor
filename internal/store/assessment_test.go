package store

import (
	"reflect"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func TestResetHistoryIsEarlierBoundedSameAuthorAndOrdered(t *testing.T) {
	s, _ := openTest(t)
	current := post("100", epoch)
	current.AuthorID = "tibo"
	older := post("8", epoch.Add(-time.Minute))
	older.AuthorID = "tibo"
	sameSecond := post("99", epoch)
	sameSecond.AuthorID = "tibo"
	sameSecond.Text = "Landing within the next 2 hours"
	sameSecond.Parent = &model.Context{ID: "8", Author: "thsottiaux", Text: "Day 10/ Reset", Available: true}
	futureID := post("101", epoch)
	futureID.AuthorID = "tibo"
	futureTime := post("9", epoch.Add(time.Second))
	futureTime.AuthorID = "tibo"
	stale := post("7", epoch.Add(-73*time.Hour))
	stale.AuthorID = "tibo"
	other := post("98", epoch)
	other.AuthorID = "someone_else"
	apply(t, s, futureID, older, other, stale, current, futureTime, sameSecond)
	for _, tc := range []struct {
		limit int
		ids   []string
	}{{8, []string{"99", "8"}}, {1, []string{"99"}}} {
		ps, err := s.RecentPosts(ctx, current, tc.limit, 72*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range ps {
			ids = append(ids, p.ID)
		}
		if !reflect.DeepEqual(ids, tc.ids) || ps[0].Parent.Text != "Day 10/ Reset" {
			t.Fatal("wrong evidence selection", ids, ps)
		}
	}
	p, err := s.Post(ctx, "99")
	if err != nil || p == nil || p.Text != sameSecond.Text {
		t.Fatal(p, err)
	}
	if p, err = s.Post(ctx, "404"); err != nil || p != nil {
		t.Fatal(p, err)
	}
	for _, n := range []int{0, 11} {
		if _, err := s.RecentPosts(ctx, current, n, time.Hour); err == nil {
			t.Fatal("unbounded history accepted")
		}
	}
}

func TestAssessmentPersistsZeroAndRejectsUnavailable(t *testing.T) {
	s, path := openTest(t)
	value := 0.0
	a := model.ResetAssessment{Probability: &value, Model: "jev-test", EvaluatedAt: epoch, HistoryCount: 2}
	if err := s.SaveResetAssessment(ctx, "post:1", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveResetAssessment(ctx, "missing", model.ResetAssessment{}); err == nil {
		t.Fatal("unavailable result was cached as a judgment")
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ResetAssessment(ctx, "post:1")
	if err != nil || got == nil || !got.Valid() || *got.Probability != 0 || !got.EvaluatedAt.Equal(epoch) {
		t.Fatal(got, err)
	}
	if got, err = reopened.ResetAssessment(ctx, "debug:1"); err != nil || got != nil {
		t.Fatal("debug reused ordinary result", got, err)
	}
}
