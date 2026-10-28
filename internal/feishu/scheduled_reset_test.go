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

func TestCodexScheduledResetCardPendingAndLiteralContent(t *testing.T) {
	for _, kind := range []string{"regular", "banked"} {
		for _, tc := range []struct {
			name    string
			planned *time.Duration
		}{
			{"unknown", nil}, {"past", durationPointer(-time.Hour)}, {"future", durationPointer(time.Hour)},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				r := *exampleCodexReport().Status.ScheduledReset
				r.ResetType, r.ScheduledFor = kind, nil
				if tc.planned != nil {
					at := r.AnnouncedAt.Add(*tc.planned)
					r.ScheduledFor = &at
				} else {
					r.Source = model.CodexSource{Type: "observed", Author: "must-not-invent-an-author"}
				}
				r.Text = "**literal** [evil](https://evil.invalid) <at id=all>all</at> <person id=x>literal</person>"
				body, err := FormatCodexScheduledReset(r)
				if err != nil {
					t.Fatal(err)
				}
				var saved cardRecord
				if json.Unmarshal([]byte(body), &saved) != nil || !reflect.DeepEqual(saved, cardRecord{Version: cardRecordVersion, CodexScheduledReset: &r}) || CardPost(body) != nil || !strings.Contains(body, `"codex_scheduled_reset":`) {
					t.Fatal("scheduled reset must persist alone, unchanged, and never be post evidence")
				}
				probability := 0.5
				if _, err := WithResetAssessment(body, model.ResetAssessment{Probability: &probability, Model: "jev-test", EvaluatedAt: r.AnnouncedAt}); err == nil {
					t.Fatal("scheduled reset acquired a TypeSafe assessment")
				}
				d := destination()
				d.Prefix = "prefix <at id=all></at>"
				payload, _ := capturedCard(t, body, d)
				card, contents := payload.Card, allContent(payload.Card)
				if card.Header.Title.Content != neutralizeMentions(d.Prefix)+" · Codex 已安排重置提醒" || payload.Sign != sign(d.Secret, payload.Timestamp) || card.Header.Template != "yellow" || len(card.Body.Elements) != 3 {
					t.Fatal("incorrect scheduled title, signature or layout")
				}
				planned, hero := "待定", "已安排 · 常规重置"
				if r.ScheduledFor != nil {
					planned = codexTimestamp(*r.ScheduledFor, beijing)
				}
				if kind == "banked" {
					hero = "已安排 · 备用重置额度发放"
				}
				for _, want := range []string{hero, "计划时间：" + planned, "尚未确认执行", "计划时间已过也不代表已完成", "不代表个人账户额度状态", "北京时间", "公告时间：" + codexTimestamp(r.AnnouncedAt, beijing), neutralizeMentions(r.Text), "记录 ID：" + r.ID, "固定公告快照", codexSourceLabel(r.Source)} {
					if !strings.Contains(contents, want) {
						t.Fatalf("missing scheduled content %q", want)
					}
				}
				for _, forbidden := range []string{"TypeSafe", "概率", "AI 预测", "已执行重置", "已发放", "<at", "DEBUG 测试", "查看原帖", "must-not-invent"} {
					if strings.Contains(contents, forbidden) {
						t.Fatalf("unexpected scheduled content %q", forbidden)
					}
				}
				attributions, links, heroes, originals := 0, 0, 0, 0
				walkElements(card.Body.Elements, func(node map[string]any) {
					content, _ := node["content"].(string)
					if content == neutralizeMentions(r.Text) {
						originals++
						if node["tag"] != "plain_text" || node["lines"] != float64(4) {
							t.Fatal("upstream text was not rendered as inert four-line plain text")
						}
					}
					if node["text_size"] == "heading-2" {
						heroes++
						if content != hero {
							t.Fatal("incorrect scheduled hero")
						}
					}
					if node["tag"] == "markdown" {
						switch content {
						case codexAttribution:
							attributions++
						case "**待执行计划**", "**计划来源**", "**数据来源**":
						default:
							t.Fatal("untrusted scheduled Markdown", content)
						}
					}
					if node["tag"] == "button" {
						links++
						if !reflect.DeepEqual(node["behaviors"], []any{map[string]any{"type": "open_url", "default_url": r.Source.Link()}}) || node["type"] == "primary_filled" {
							t.Fatal("scheduled source button must be native and secondary")
						}
					}
				})
				if attributions != 1 || links != 1 || heroes != 1 || originals != 1 {
					t.Fatal("scheduled card lost its attribution, source, hero or original text")
				}
			})
		}
	}
}

func TestCodexScheduledResetDurableRoundTripIgnoresDeliveryClock(t *testing.T) {
	r := *exampleCodexReport().Status.ScheduledReset
	body, err := FormatCodexScheduledReset(r)
	if err != nil {
		t.Fatal(err)
	}
	saved, isCard, err := decodeCard(body)
	if err != nil || !isCard || !reflect.DeepEqual(saved.CodexScheduledReset, &r) {
		t.Fatal("scheduled reset did not survive durable decoding", err)
	}
	if again, err := encodeCard(saved); err != nil || again != body {
		t.Fatal("scheduled round trip changed the queued record", err)
	}
	c := New(time.Second)
	var first cardPayload
	for i, at := range []time.Time{r.ScheduledFor.Add(-time.Hour), r.ScheduledFor.Add(48 * time.Hour)} {
		c.now = func() time.Time { return at }
		wire, err := c.cardBody("saved plan", "retry-secret", saved)
		var got cardPayload
		if err != nil || json.Unmarshal(wire, &got) != nil || got.Sign != sign("retry-secret", got.Timestamp) {
			t.Fatal("scheduled retry encoding/signing failed", err)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(first.Card, got.Card) || first.Timestamp == got.Timestamp || first.Sign == got.Sign {
			t.Fatal("delivery clock changed the saved plan or failed to refresh signing")
		}
	}
	wire, err := c.cardBody("rotated prefix", "rotated-secret", saved)
	var rotated cardPayload
	if err != nil || json.Unmarshal(wire, &rotated) != nil || rotated.Sign != sign("rotated-secret", rotated.Timestamp) || !strings.HasPrefix(rotated.Card.Header.Title.Content, "rotated prefix") || !reflect.DeepEqual(first.Card.Body, rotated.Card.Body) {
		t.Fatal("credential/prefix rotation changed the saved plan", err)
	}
	if again, err := encodeCard(saved); err != nil || again != body {
		t.Fatal("rendering mutated durable plan", err)
	}
}

func TestCodexScheduledResetInvalidAndMixedRecordsRejectedOffline(t *testing.T) {
	for _, mutate := range []func(*model.CodexScheduledReset){
		func(r *model.CodexScheduledReset) { r.ID = "private\nbad-id" },
		func(r *model.CodexScheduledReset) { r.Status = "completed" },
		func(r *model.CodexScheduledReset) { r.Status = "" },
		func(r *model.CodexScheduledReset) { r.ResetType = "private-invalid" },
		func(r *model.CodexScheduledReset) { r.AnnouncedAt = time.Time{} },
		func(r *model.CodexScheduledReset) { r.ScheduledFor = &time.Time{} },
		func(r *model.CodexScheduledReset) { r.Text = "\xffprivate-text" },
		func(r *model.CodexScheduledReset) { r.Text = strings.Repeat("x", (1<<20)+1) },
		func(r *model.CodexScheduledReset) { r.Source.Type = "private-invalid" },
		func(r *model.CodexScheduledReset) { r.Source.URL = "https://private.invalid/" },
		func(r *model.CodexScheduledReset) { r.Source.Author = "fake" },
	} {
		r := *exampleCodexReport().Status.ScheduledReset
		mutate(&r)
		if body, err := FormatCodexScheduledReset(r); body != "" || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid scheduled format did not fail safely", err)
		}
	}
	r, post, reset, report := *exampleCodexReport().Status.ScheduledReset, examplePost(), exampleCodexReset(), exampleCodexReport()
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid scheduled reset reached transport")
		return nil, nil
	})
	for _, record := range []cardRecord{
		{Version: cardRecordVersion, CodexScheduledReset: &model.CodexScheduledReset{}},
		{Version: cardRecordVersion, CodexScheduledReset: &r, Post: &post},
		{Version: cardRecordVersion, CodexScheduledReset: &r, Reset: &model.ResetAssessment{}},
		{Version: cardRecordVersion, CodexScheduledReset: &r, CodexReset: &reset},
		{Version: cardRecordVersion, CodexScheduledReset: &r, CodexWatch: report.Status.ActiveWatch},
		{Version: cardRecordVersion, CodexScheduledReset: &r, CodexReport: &report},
		{Version: cardRecordVersion, CodexScheduledReset: &r, Notice: "private-notice"},
		{Version: cardRecordVersion, CodexScheduledReset: &r, Debug: true},
	} {
		if body, err := encodeCard(record); body != "" || err == nil {
			t.Fatal("encoder accepted invalid or mixed scheduled payload")
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		sendError(t, c.Send(context.Background(), destination(), string(raw)), "invalid_request", true)
		if CardPost(string(raw)) != nil {
			t.Fatal("mixed scheduled reset exposed post evidence")
		}
	}
	for _, raw := range []string{`{"tibo_card":"tibo.post-card.v1","codex_scheduled_reset":42}`, `{"tibo_card":"tibo.post-card.v1","codex_scheduled_reset":{}}`} {
		sendError(t, c.Send(context.Background(), destination(), raw), "invalid_request", true)
	}
}

func TestCodexScheduledResetLongTextBoundsSignedWireNotDurableRecord(t *testing.T) {
	for _, text := range []string{strings.Repeat("abc", 30000), strings.Repeat("中文é", 10000), strings.Repeat("\x00\x01\"\\\n<>&\u2028\u2029", 5000), strings.Repeat("<at id=all>", 10000)} {
		r := *exampleCodexReport().Status.ScheduledReset
		r.Text = text
		body, err := FormatCodexScheduledReset(r)
		if err != nil {
			t.Fatal(err)
		}
		var saved cardRecord
		if json.Unmarshal([]byte(body), &saved) != nil || !reflect.DeepEqual(saved.CodexScheduledReset, &r) {
			t.Fatal("durable scheduled text was truncated")
		}
		d := destination()
		d.Prefix = strings.Repeat("\x00", maxPrefixBytes)
		payload, wire := capturedCard(t, body, d)
		if len(wire) > maxCardRequestBytes || payload.Sign != sign(d.Secret, payload.Timestamp) || !strings.Contains(allContent(payload.Card), "已截断") || strings.Contains(allContent(payload.Card), "<at") {
			t.Fatal("scheduled wire budget, signature, truncation notice or mention guard failed")
		}
		found := false
		walkElements(payload.Card.Body.Elements, func(node map[string]any) {
			if node["tag"] == "plain_text" && node["lines"] == float64(4) {
				found = true
				snippet := strings.ReplaceAll(node["content"].(string), "<\u200bat", "<at")
				if !utf8.ValidString(snippet) || snippet == "" || len(snippet) >= len(text) || !strings.HasPrefix(text, snippet) {
					t.Fatal("scheduled truncation corrupted the original UTF-8 excerpt")
				}
			}
		})
		if !found {
			t.Fatal("scheduled reset lost its original-text excerpt")
		}
	}
}

func TestCodexScheduledResetEmptyText(t *testing.T) {
	r := *exampleCodexReport().Status.ScheduledReset
	r.Text = ""
	body, err := FormatCodexScheduledReset(r)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := capturedCard(t, body, destination())
	if !strings.Contains(allContent(payload.Card), "上游未提供文字说明。") || strings.Contains(allContent(payload.Card), "已截断") {
		t.Fatal("empty scheduled text must remain explicitly unavailable")
	}
	raw, err := json.Marshal(renderCodexScheduledReset(r, "prefix", "", true))
	if err != nil {
		t.Fatal(err)
	}
	var card cardView
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(allContent(card), "上游正文已省略，请查看来源。") {
		t.Fatal("fully truncated text was mistaken for missing upstream text")
	}
}

func TestCodexScheduledResetFieldKeepsExistingRecordsCompatible(t *testing.T) {
	post, reset, report := examplePost(), exampleCodexReset(), exampleCodexReport()
	for _, record := range []cardRecord{
		{Version: cardRecordVersion},
		{Version: cardRecordVersion, Notice: "old notice"},
		{Version: cardRecordVersion, Post: &post, Debug: true},
		{Version: cardRecordVersion, CodexReset: &reset, Debug: true},
		{Version: cardRecordVersion, CodexWatch: report.Status.ActiveWatch},
		{Version: cardRecordVersion, CodexReport: &report},
	} {
		body, err := encodeCard(record)
		if err != nil || strings.Contains(body, "codex_scheduled_reset") {
			t.Fatal("new optional field changed existing record encoding", err)
		}
		got, isCard, err := decodeCard(body)
		if err != nil || !isCard || !reflect.DeepEqual(got, record) {
			t.Fatal("existing record decoding changed", err)
		}
		capturedCard(t, body, destination())
	}
	capturedCard(t, legacyDebugPrefix+" old queued debug", destination())
}
