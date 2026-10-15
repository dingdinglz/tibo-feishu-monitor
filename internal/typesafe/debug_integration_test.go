package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/monitor"
	"tibo-monitor/internal/store"
)

type debugFetcher struct{ post model.Post }

func (f debugFetcher) Fetch(_ context.Context, _ model.FetchRequest) (model.FetchResult, error) {
	return model.FetchResult{HeadOK: true, Complete: true, Latest: &f.post}, nil
}

// Exercise the production debug -> queue -> HTTP TypeSafe -> HTTP Feishu path.
// Only network endpoints are redirected to local servers; the assessor and card
// sender are the real implementations, not test doubles with pre-filled cards.
func TestDebugCallsTypeSafeBeforeSendingRenderedProbability(t *testing.T) {
	var evaluations atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evaluations.Add(1)
		var request struct {
			State evidenceState `json:"state"`
		}
		if r.Header.Get("Authorization") != "Bearer "+testKey || json.NewDecoder(r.Body).Decode(&request) != nil || request.State.CurrentPost.Text != "Done and dusted" {
			t.Error("debug did not invoke TypeSafe with the actual post")
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, answerBody("0.91"))
	}))
	defer provider.Close()
	cards := make(chan string, 1)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || evaluations.Load() != 1 {
			t.Error("webhook called before inference")
		}
		select {
		case cards <- string(body):
		default:
		}
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer webhook.Close()

	c := config.Defaults()
	c.Webhooks = []config.Destination{{ID: "local-test", URL: webhook.URL}}
	s, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SyncDestinations(context.Background(), c.Target.UserID, c.Webhooks); err != nil {
		t.Fatal(err)
	}
	assessor := newTestClient(t)
	routeToServer(t, assessor, provider)
	p := currentPost()
	p.Text, p.Kind, p.AuthorID = "Done and dusted", "quote", c.Target.UserID
	p.CreatedAt = time.Now().UTC().Add(-48 * time.Hour) // debug outside catch-up still calls API
	p.Quote = &model.Context{ID: "99", Author: "thsottiaux", Text: "Reset landing in the next 2 hours", Available: true}
	m := &monitor.Monitor{Config: c, Store: s, Fetcher: debugFetcher{p}, Sender: feishu.New(time.Second),
		Assessor: assessor, Debug: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	var body string
	select {
	case body = <-cards:
	case <-ctx.Done():
		t.Error("debug did not produce a card")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg_type":"interactive"`, `"schema":"2.0"`, "DEBUG 测试", "本次额度重置概率", "█████████░  91%", "jev-1.13.0", "Done and dusted"} {
		if !strings.Contains(body, want) {
			t.Errorf("actual webhook card is missing %q", want)
		}
	}
	if strings.Contains(body, testKey) || strings.Contains(body, "暂不可用") || evaluations.Load() != 1 {
		t.Fatal("debug substituted/omitted the judgment or leaked credentials")
	}
}
