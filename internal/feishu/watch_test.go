package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tibo-monitor/internal/model"
)

func TestCodexWatchCardProbabilityAndLiteralContent(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		chance     *int
	}{
		{"upstream", "73%", intPointer(73)}, {"unknown", "未知", nil},
		{"zero", "0%", intPointer(0)}, {"certain", "100%", intPointer(100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := *exampleCodexReport().Status.ActiveWatch
			w.ResetChancePercent = tc.chance
			if tc.chance != nil {
				w.Source = exampleCodexReset().Source
			} else {
				w.Level = "elevated"
			}
			w.Text = "**literal** [evil](https://evil.invalid) <at id=all>all</at> <person id=x>literal</person>"
			w.ForecastWindow = "next 12h **window** <at id=all></at>"
			body, err := FormatCodexWatch(w)
			if err != nil {
				t.Fatal(err)
			}
			var saved cardRecord
			if json.Unmarshal([]byte(body), &saved) != nil || !reflect.DeepEqual(saved, cardRecord{Version: cardRecordVersion, CodexWatch: &w}) || CardPost(body) != nil {
				t.Fatal("watch must persist alone, unchanged, and never be post evidence")
			}
			probability := 0.5
			if _, err := WithResetAssessment(body, model.ResetAssessment{Probability: &probability, Model: "jev-test", EvaluatedAt: w.ObservedAt}); err == nil {
				t.Fatal("watch acquired a TypeSafe assessment")
			}
			d := destination()
			payload, _ := capturedCard(t, body, d)
			contents := allContent(payload.Card)
			if !strings.HasSuffix(payload.Card.Header.Title.Content, "Codex 可能重置提醒") || payload.Sign != sign(d.Secret, payload.Timestamp) {
				t.Fatal("incorrect watch title or signature")
			}
			for _, want := range []string{"可能重置", tc.want, "预测窗口", "观测", "有效期至", "北京时间", codexTimestamp(w.ObservedAt, beijing), codexTimestamp(w.ExpiresAt, beijing), neutralizeMentions(w.Text), neutralizeMentions(w.ForecastWindow), "第三方 AI 预测，非官方承诺；不表示已执行重置。"} {
				if !strings.Contains(contents, want) {
					t.Fatalf("missing watch content %q", want)
				}
			}
			for _, forbidden := range []string{"TypeSafe", "本次额度重置概率", "<at", "DEBUG 测试", "查看原帖"} {
				if strings.Contains(contents, forbidden) {
					t.Fatalf("unexpected watch content %q", forbidden)
				}
			}
			if (tc.chance == nil && strings.Contains(contents, "0%")) || (tc.chance != nil && strings.Contains(contents, "未知")) {
				t.Fatal("unknown and zero probability were conflated")
			}
			attributions, links := 0, 0
			walkElements(payload.Card.Body.Elements, func(node map[string]any) {
				content, _ := node["content"].(string)
				if strings.Contains(content, "literal") || strings.Contains(content, "**window**") {
					if node["tag"] != "plain_text" {
						t.Fatal("upstream markup was not rendered as inert plain text")
					}
				}
				if node["tag"] == "markdown" && content == codexAttribution {
					attributions++
				}
				if node["tag"] == "button" {
					links++
					if !reflect.DeepEqual(node["behaviors"], []any{map[string]any{"type": "open_url", "default_url": w.Source.Link()}}) {
						t.Fatal("watch link is not the native, validated source URL")
					}
				}
			})
			if attributions != 1 || links != 1 {
				t.Fatal("watch needs exactly one fixed attribution and one source button")
			}
		})
	}
}

func TestCodexWatchDurableRoundTripIgnoresDeliveryClock(t *testing.T) {
	w := *exampleCodexReport().Status.ActiveWatch
	body, err := FormatCodexWatch(w)
	if err != nil {
		t.Fatal(err)
	}
	saved, isCard, err := decodeCard(body)
	if err != nil || !isCard || !reflect.DeepEqual(saved.CodexWatch, &w) {
		t.Fatal("watch did not survive durable decoding", err)
	}
	if again, err := encodeCard(saved); err != nil || again != body {
		t.Fatal("watch round trip changed the queued record", err)
	}
	c := New(time.Second)
	var first cardPayload
	for i, at := range []time.Time{w.ObservedAt, w.ExpiresAt.Add(48 * time.Hour)} {
		c.now = func() time.Time { return at }
		wire, err := c.cardBody("saved watch", "retry-secret", saved) // Pure rendering; no HTTP.
		var got cardPayload
		if err != nil || json.Unmarshal(wire, &got) != nil || got.Sign != sign("retry-secret", got.Timestamp) {
			t.Fatal("watch retry encoding/signing failed", err)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(first.Card, got.Card) || first.Timestamp == got.Timestamp || first.Sign == got.Sign {
			t.Fatal("delivery clock changed the saved watch or failed to refresh signing")
		}
	}
}

func TestCodexWatchInvalidAndMixedRecordsRejectedOffline(t *testing.T) {
	for _, mutate := range []func(*model.CodexWatch){
		func(w *model.CodexWatch) { w.Level = "private-invalid" },
		func(w *model.CodexWatch) { w.ResetChancePercent = intPointer(-1) },
		func(w *model.CodexWatch) { w.ResetChancePercent = intPointer(101) },
		func(w *model.CodexWatch) { w.ObservedAt = time.Time{} },
		func(w *model.CodexWatch) { w.ExpiresAt = w.ObservedAt },
		func(w *model.CodexWatch) { w.Text = "\xffprivate-text" },
		func(w *model.CodexWatch) { w.Text = strings.Repeat("x", (1<<20)+1) },
		func(w *model.CodexWatch) { w.ForecastWindow = "\xffprivate-window" },
		func(w *model.CodexWatch) { w.ForecastWindow = strings.Repeat("x", 1025) },
		func(w *model.CodexWatch) { w.Source.Type = "private-invalid" },
		func(w *model.CodexWatch) { w.Source.URL = "https://private.invalid/" },
	} {
		w := *exampleCodexReport().Status.ActiveWatch
		mutate(&w)
		if body, err := FormatCodexWatch(w); body != "" || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid watch format did not fail safely", err)
		}
	}
	w, post, reset, report := *exampleCodexReport().Status.ActiveWatch, examplePost(), exampleCodexReset(), exampleCodexReport()
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid watch reached transport")
		return nil, nil
	})
	for _, record := range []cardRecord{
		{Version: cardRecordVersion, CodexWatch: &model.CodexWatch{}},
		{Version: cardRecordVersion, CodexWatch: &w, Post: &post},
		{Version: cardRecordVersion, CodexWatch: &w, Reset: &model.ResetAssessment{}},
		{Version: cardRecordVersion, CodexWatch: &w, CodexReset: &reset},
		{Version: cardRecordVersion, CodexWatch: &w, CodexReport: &report},
		{Version: cardRecordVersion, CodexWatch: &w, Notice: "private-notice"},
		{Version: cardRecordVersion, CodexWatch: &w, Debug: true},
	} {
		if body, err := encodeCard(record); body != "" || err == nil {
			t.Fatal("encoder accepted invalid or mixed watch payload")
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		sendError(t, c.Send(context.Background(), destination(), string(raw)), "invalid_request", true)
		if CardPost(string(raw)) != nil {
			t.Fatal("mixed watch exposed post evidence")
		}
	}
	for _, raw := range []string{`{"tibo_card":"tibo.post-card.v1","codex_watch":42}`, `{"tibo_card":"tibo.post-card.v1","codex_watch":{}}`} {
		sendError(t, c.Send(context.Background(), destination(), raw), "invalid_request", true)
	}
}

func TestCodexWatchLongTextBoundsSignedWireNotDurableRecord(t *testing.T) {
	for _, text := range []string{strings.Repeat("abc", 30000), strings.Repeat("中文é", 10000), strings.Repeat("\x00\x01\"\\\n<>&\u2028\u2029", 5000), strings.Repeat("<at id=all>", 10000)} {
		w := *exampleCodexReport().Status.ActiveWatch
		w.Text = text
		body, err := FormatCodexWatch(w)
		if err != nil {
			t.Fatal(err)
		}
		var saved cardRecord
		if json.Unmarshal([]byte(body), &saved) != nil || !reflect.DeepEqual(saved.CodexWatch, &w) {
			t.Fatal("durable watch text was truncated")
		}
		d := destination()
		d.Prefix = strings.Repeat("\x00", maxPrefixBytes)
		payload, wire := capturedCard(t, body, d)
		if len(wire) > maxCardRequestBytes || payload.Sign != sign(d.Secret, payload.Timestamp) || !strings.Contains(allContent(payload.Card), "已截断") || strings.Contains(allContent(payload.Card), "<at") {
			t.Fatal("watch wire budget, signature, truncation notice or mention guard failed")
		}
		found := false
		walkElements(payload.Card.Body.Elements, func(node map[string]any) {
			if node["tag"] == "plain_text" && node["lines"] == float64(4) {
				found = true
				snippet := strings.ReplaceAll(node["content"].(string), "<\u200bat", "<at")
				if !utf8.ValidString(snippet) || snippet == "" || len(snippet) >= len(text) || !strings.HasPrefix(text, snippet) {
					t.Fatal("watch truncation corrupted the original UTF-8 excerpt")
				}
			}
		})
		if !found {
			t.Fatal("watch lost its original-text excerpt")
		}
	}
}
