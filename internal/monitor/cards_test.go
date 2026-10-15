package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func TestMultipleNewPostsEachQueueOneIndependentCard(t *testing.T) {
	m, f := newTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	f.result = model.FetchResult{HeadOK: true, Complete: true}
	if _, err := m.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	posts := []model.Post{
		fixture("101", now.Add(time.Minute)), fixture("102", now.Add(2*time.Minute)),
		fixture("103", now.Add(3*time.Minute)), fixture("104", now.Add(4*time.Minute)),
	}
	posts[0].Kind = "post"
	posts[1].Kind = "reply"
	posts[2].Kind = "quote"
	posts[3].Text = strings.Repeat("超长正文🙂", 20000)
	f.result.Posts = []model.Post{posts[3], posts[1], posts[2], posts[0]}
	out, err := m.Poll(ctx, now.Add(5*time.Minute))
	if err != nil || out.Added != 4 {
		t.Fatal(out, err)
	}
	st, err := m.Store.Status(ctx)
	if err != nil || st.Pending != 8 {
		t.Fatalf("want exactly 4 cards per target, got %+v: %v", st, err)
	}
	for _, dest := range []string{"a", "b"} {
		for i, want := range posts {
			at := now.Add(time.Duration(6+i) * time.Minute)
			job, err := m.Store.Claim(ctx, dest, at, time.Second)
			if err != nil || job == nil || job.Kind != "post" {
				t.Fatal(job, err)
			}
			var card struct {
				Version string     `json:"tibo_card"`
				Post    model.Post `json:"post"`
				Debug   bool       `json:"debug"`
			}
			if err := json.Unmarshal([]byte(job.Text), &card); err != nil {
				t.Fatal("queue does not contain a card record", err)
			}
			if card.Version != "tibo.post-card.v1" || card.Debug || card.Post.ID != want.ID || card.Post.Text != want.Text {
				t.Fatal("card merged, split or reordered posts", card.Post.ID)
			}
			if err := m.Store.Sent(ctx, *job, at); err != nil {
				t.Fatal(err)
			}
		}
		if job, err := m.Store.Claim(ctx, dest, now.Add(10*time.Minute), time.Second); err != nil || job != nil {
			t.Fatal("extra card", job, err)
		}
	}
	if _, err := m.Poll(ctx, now.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, err = m.Store.Status(ctx)
	if err != nil || st.Pending != 0 || st.Sent != 8 {
		t.Fatal("repeat scan duplicated cards", st, err)
	}
}
