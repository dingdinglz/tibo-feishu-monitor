package feishu

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tibo-monitor/internal/model"
)

func exampleCodexReset() model.CodexReset {
	return model.CodexReset{
		ID: "api-event_1", ResetType: "regular", AnnouncedAt: time.Date(2026, 7, 11, 22, 5, 6, 0, time.UTC),
		Text:   "Upstream reset announcement",
		Source: model.CodexSource{Type: "x_post", Author: "thsottiaux", URL: "https://twitter.com/thsottiaux/status/123?ref=upstream"},
	}
}

func exampleCodexReport() model.CodexReport {
	latest := exampleCodexReset()
	asOf := latest.AnnouncedAt.Add(6 * time.Hour)
	planned := asOf.Add(-time.Hour)
	avg, longest, roundedDays := 2.5, 7.5, 999.0
	chance := 73
	return model.CodexReport{
		AsOf: asOf, Date: "2026-07-12", Timezone: "Asia/Shanghai", LongestIntervalDays: &longest,
		Status: model.CodexStatus{
			GeneratedAt: asOf.Add(-2 * time.Minute), LatestReset: &latest,
			Stats: model.CodexStats{Total: 37, LastResetAt: &latest.AnnouncedAt, DaysSinceLast: &roundedDays, AvgIntervalDays: &avg},
			ScheduledReset: &model.CodexScheduledReset{
				ID: "scheduled_1", Status: "scheduled", ResetType: "regular", AnnouncedAt: asOf.Add(-2 * time.Hour),
				ScheduledFor: &planned, Text: "Scheduled upstream explanation", Source: latest.Source,
			},
			ActiveWatch: &model.CodexWatch{
				Level: "strong", ResetChancePercent: &chance, ForecastWindow: "next 12 hours", ObservedAt: asOf.Add(-time.Hour),
				ExpiresAt: asOf.Add(time.Hour), Text: "Upstream watch explanation", Source: model.CodexSource{Type: "observed"},
			},
		},
	}
}

func assertCodexCard(t *testing.T, card cardView, sources ...model.CodexSource) {
	t.Helper()
	contents := allContent(card)
	for _, forbidden := range []string{"TypeSafe", "本次额度重置概率", "查看原帖", "28天挑战", "28 天挑战", "<at"} {
		if strings.Contains(contents, forbidden) {
			t.Fatalf("API card contains %q", forbidden)
		}
	}
	if len(card.Body.Elements) < 2 || len(card.Body.Elements) > 5 {
		t.Fatal("API card must have 2–5 visual blocks")
	}
	attributions, links := 0, 0
	walkElements(card.Body.Elements, func(node map[string]any) {
		if node["tag"] == "markdown" {
			content := node["content"].(string)
			switch content {
			case codexAttribution:
				attributions++
			case "**重置状态**", "**上游记录**", "**数据来源**", "**最近重置**", "**待执行计划**", "**AI 预测**", "**待执行 / AI 预测**", "**快照时间**":
			default:
				t.Fatalf("untrusted API Markdown: %q", content)
			}
		}
		if node["tag"] == "column_set" && node["flex_mode"] != "none" {
			t.Fatal("mobile columns should not stretch")
		}
		if node["tag"] == "button" {
			behaviors := node["behaviors"].([]any)
			if len(behaviors) != 1 || behaviors[0].(map[string]any)["type"] != "open_url" {
				t.Fatal("API cards must not create callbacks")
			}
			url := behaviors[0].(map[string]any)["default_url"]
			if links >= len(sources) || url != sources[links].Link() {
				t.Fatalf("source link not taken from CodexSource.Link: %v", url)
			}
			links++
		}
	})
	if attributions != 1 || links != len(sources) {
		t.Fatalf("missing attribution or sources: attribution=%d sources=%d", attributions, links)
	}
}

func reportCard(t *testing.T, r model.CodexReport) cardView {
	t.Helper()
	body, err := FormatCodexReport(r)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := capturedCard(t, body, destination())
	return payload.Card
}

func TestCodexResetTypedRecordsAndEventKinds(t *testing.T) {
	for _, kind := range []string{"regular", "banked"} {
		for _, observed := range []bool{false, true} {
			for _, debug := range []bool{false, true} {
				t.Run(kind+"/observed="+strconv.FormatBool(observed)+"/debug="+strconv.FormatBool(debug), func(t *testing.T) {
					r := exampleCodexReset()
					r.ResetType = kind
					r.Text = "**literal** [not a link](https://untrusted.invalid) <at id=all>all</at>"
					if observed {
						r.Source = model.CodexSource{Type: "observed", Author: "must-not-invent-an-author"}
					}
					body, err := FormatCodexReset(r, debug)
					if err != nil {
						t.Fatal(err)
					}
					var saved cardRecord
					if err := json.Unmarshal([]byte(body), &saved); err != nil || !reflect.DeepEqual(saved.CodexReset, &r) || saved.Post != nil || saved.Reset != nil || saved.CodexReport != nil || saved.Debug != debug {
						t.Fatal("event must persist as a typed record, not a post", err)
					}
					if CardPost(body) != nil {
						t.Fatal("API event must bypass post assessments")
					}
					probability := 0.5
					assessment := model.ResetAssessment{Probability: &probability, Model: "jev-test", EvaluatedAt: r.AnnouncedAt}
					if _, err := WithResetAssessment(body, assessment); err == nil {
						t.Fatal("API event acquired an assessment")
					}
					payload, _ := capturedCard(t, body, destination())
					assertCodexCard(t, payload.Card, r.Source)
					contents := allContent(payload.Card)
					for _, want := range []string{neutralizeMentions(r.Text), codexResetState(kind), "据 Codex Resets 上游记录", codexTimestamp(r.AnnouncedAt, beijing), r.ID} {
						if !strings.Contains(contents, want) {
							t.Fatalf("missing event content %q", want)
						}
					}
					if kind == "banked" && strings.Contains(contents, "已执行重置") {
						t.Fatal("banked credit described as an executed reset")
					}
					if observed && (!strings.Contains(contents, "网站观测") || strings.Contains(contents, "@") || strings.Contains(contents, "must-not-invent")) {
						t.Fatal("observed event invented an author")
					}
					if debug != strings.Contains(contents, "DEBUG 测试") || (debug && !strings.Contains(contents, "不是新增通知")) {
						t.Fatal("debug replay lacks an explicit banner")
					}
				})
			}
		}
	}
}

func TestCodexDailySnapshotHTTPRetriesAndVisualHierarchy(t *testing.T) {
	r := exampleCodexReport()
	body, err := FormatCodexReport(r)
	if err != nil {
		t.Fatal(err)
	}
	var saved cardRecord
	if err := json.Unmarshal([]byte(body), &saved); err != nil || !reflect.DeepEqual(saved.CodexReport, &r) || saved.Post != nil || saved.Reset != nil || saved.CodexReset != nil || CardPost(body) != nil {
		t.Fatal("daily report did not retain its typed snapshot", err)
	}
	attempts := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wire, err := io.ReadAll(req.Body)
		if err != nil || len(wire) > maxCardRequestBytes || int64(len(wire)) != req.ContentLength || req.Header.Get("Content-Type") != "application/json" {
			t.Error("invalid actual HTTP request", err)
		}
		attempts <- wire
		if len(attempts) == 1 {
			_, _ = io.WriteString(w, `{"code":11232}`)
		} else {
			_, _ = io.WriteString(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	client := New(time.Second)
	defer client.httpClient.CloseIdleConnections()
	// The watch has expired by delivery time, but was active in this snapshot.
	now := r.AsOf.Add(48 * time.Hour)
	client.now = func() time.Time { return now }
	d := destination()
	d.URL, d.Secret = srv.URL, "first-secret"
	sendError(t, client.Send(context.Background(), d, body), "11232", false)
	now = now.Add(time.Second)
	d.Secret, d.Prefix = "rotated-secret", "new keyword"
	if err := client.Send(context.Background(), d, body); err != nil {
		t.Fatal(err)
	}
	var first, second cardPayload
	if json.Unmarshal(<-attempts, &first) != nil || json.Unmarshal(<-attempts, &second) != nil {
		t.Fatal("invalid card JSON")
	}
	if first.MsgType != "interactive" || first.Card.Schema != "2.0" || first.Card.Config.WidthMode != "default" || first.Sign != sign("first-secret", first.Timestamp) || second.Sign != sign(d.Secret, second.Timestamp) || first.Sign == second.Sign || first.Timestamp == second.Timestamp {
		t.Fatal("reports did not use the existing signed Card 2.0 transport")
	}
	if !strings.HasPrefix(second.Card.Header.Title.Content, d.Prefix) || !reflect.DeepEqual(first.Card.Body, second.Card.Body) {
		t.Fatal("retry did not refresh prefix or changed the durable report")
	}
	card := second.Card
	assertCodexCard(t, card, r.Status.LatestReset.Source, r.Status.ScheduledReset.Source, r.Status.ActiveWatch.Source)
	if card.Header.Template != "yellow" || card.Header.Subtitle == nil || card.Header.Subtitle.Content != "2026-07-12 · Asia/Shanghai" || card.Header.Icon["token"] != "calendar_colorful" {
		t.Fatal("missing yellow header/date/timezone/icon")
	}
	contents := allContent(card)
	for _, want := range []string{"距最近重置 6小时", "37次", "2.5天", "7.5天", "重置次数", "平均间隔", "最长等待", "待执行", "计划时间已过，尚未确认执行", "重置概率：73%", "AI 预测，非官方承诺", "抓取 / 截至：" + codexTimestamp(r.AsOf, beijing), "上游生成：" + codexTimestamp(r.Status.GeneratedAt, beijing), "固定快照"} {
		if !strings.Contains(contents, want) {
			t.Fatalf("missing daily content %q", want)
		}
	}
	if strings.Contains(contents, "999") || len(card.Body.Elements) != 4 {
		t.Fatal("used rounded upstream days or failed to combine upcoming information")
	}
	hero := card.Body.Elements[0].(map[string]any)["columns"].([]any)[0].(map[string]any)
	if hero["background_style"] != "yellow-50" {
		t.Fatal("latest reset must be the warm yellow highlight")
	}
	tiles := card.Body.Elements[1].(map[string]any)
	columns := tiles["columns"].([]any)
	if tiles["flex_mode"] != "none" || len(columns) != 3 {
		t.Fatal("daily stats need three mobile-safe columns")
	}
	for _, node := range columns {
		column := node.(map[string]any)
		if column["width"] != "weighted" || column["weight"] != float64(1) || column["background_style"] != "grey-50" {
			t.Fatal("stats must be secondary weighted tiles")
		}
	}
	focus := 0
	walkElements(card.Body.Elements, func(node map[string]any) {
		if size, ok := node["text_size"].(string); ok && strings.HasPrefix(size, "heading-") {
			focus++
			if size != "heading-2" || node["content"] != "距最近重置 6小时" {
				t.Fatal("secondary information competes with the hero")
			}
		}
		if node["type"] == "primary_filled" {
			t.Fatal("report source links should not compete with the hero")
		}
	})
	if focus != 1 {
		t.Fatal("report must have exactly one strongest focus")
	}
}

func TestCodexReportNullStatusAndStats(t *testing.T) {
	var r model.CodexReport
	if err := json.Unmarshal([]byte(`{"as_of":"2026-07-12T04:05:06Z","date":"2026-07-12","timezone":"Asia/Shanghai","longest_interval_days":null,"status":{"latest_reset":null,"scheduled_reset":null,"active_watch":null,"generated_at":"2026-07-12T04:04:00Z","stats":{"total":0,"last_reset_at":null,"days_since_last":null,"avg_interval_days":null}}}`), &r); err != nil {
		t.Fatal(err)
	}
	card := reportCard(t, r)
	assertCodexCard(t, card)
	contents := allContent(card)
	if !strings.Contains(contents, "距最近重置 暂无数据") || !strings.Contains(contents, "0次") || strings.Count(contents, "暂无数据") != 3 || strings.Contains(contents, "%") || len(card.Body.Elements) != 3 {
		t.Fatal("null status/stats were invented or displayed as zero probability", contents)
	}
}

func TestCodexReportElapsedUsesAsOfNotRoundedDays(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		elapsed    time.Duration
		statsOnly  bool
	}{
		{"minutes", "不足1小时", 30 * time.Minute, false},
		{"hours", "23小时", 23 * time.Hour, false},
		{"days", "1.5天", 36 * time.Hour, false},
		{"stats-fallback", "6小时", 6 * time.Hour, true},
		{"future", "暂无数据", -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleCodexReport()
			at := r.AsOf.Add(-tc.elapsed)
			r.Status.LatestReset.AnnouncedAt = at
			r.Status.Stats.LastResetAt = &at
			if tc.statsOnly {
				r.Status.LatestReset = nil
			}
			card := reportCard(t, r)
			if !strings.Contains(allContent(card), "距最近重置 "+tc.want) || strings.Contains(allContent(card), "999天") {
				t.Fatal("elapsed time not derived from snapshot timestamp")
			}
		})
	}
}

func TestCodexScheduledRemainsPendingAndWatchRequiresUnexpiredSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		planned    *time.Duration
		kind, want string
	}{
		{"past", durationPointer(-time.Hour), "regular", "计划时间已过，尚未确认执行"},
		{"now", durationPointer(0), "regular", "计划时间已过，尚未确认执行"},
		{"future-banked", durationPointer(time.Hour), "banked", "以上为上游计划，尚未确认执行"},
		{"unknown-date", nil, "regular", "计划时间：待定；尚未确认执行"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleCodexReport()
			r.Status.LatestReset, r.Status.ActiveWatch, r.Status.Stats.LastResetAt = nil, nil, nil
			r.Status.ScheduledReset.ResetType = tc.kind
			r.Status.ScheduledReset.ScheduledFor = nil
			if tc.planned != nil {
				at := r.AsOf.Add(*tc.planned)
				r.Status.ScheduledReset.ScheduledFor = &at
			}
			card := reportCard(t, r)
			assertCodexCard(t, card, r.Status.ScheduledReset.Source)
			contents := allContent(card)
			if !strings.Contains(contents, tc.want) || !strings.Contains(contents, "待执行") || strings.Contains(contents, "已执行重置") || strings.Contains(contents, "已发放") || strings.Contains(contents, "AI 预测") {
				t.Fatal("scheduled reset presented as executed or as a prediction", contents)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		expires time.Duration
		chance  *int
		want    string
	}{
		{"unknown", time.Hour, nil, "重置概率：未知"},
		{"zero", time.Hour, intPointer(0), "重置概率：0%"},
		{"certain", time.Hour, intPointer(100), "重置概率：100%"},
		{"expiry-is-exclusive", 0, nil, ""},
		{"expired", -time.Minute, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := exampleCodexReport()
			r.Status.ActiveWatch.ResetChancePercent = tc.chance
			r.Status.ActiveWatch.ExpiresAt = r.AsOf.Add(tc.expires)
			card := reportCard(t, r)
			sources := []model.CodexSource{r.Status.LatestReset.Source, r.Status.ScheduledReset.Source}
			if tc.want != "" {
				sources = append(sources, r.Status.ActiveWatch.Source)
			}
			assertCodexCard(t, card, sources...)
			contents := allContent(card)
			if tc.want == "" {
				if strings.Contains(contents, "AI 预测") || strings.Contains(contents, r.Status.ActiveWatch.Text) || strings.Contains(contents, "概率") {
					t.Fatal("expired watch rendered in the report")
				}
			} else if !strings.Contains(contents, tc.want) || !strings.Contains(contents, "AI 预测，非官方承诺") || (tc.chance == nil && strings.Contains(contents, "0%")) {
				t.Fatal("watch probability/disclaimer incorrect", contents)
			}
		})
	}
	// A watch without a schedule still gets one distinct secondary section.
	r := exampleCodexReport()
	r.Status.ScheduledReset = nil
	card := reportCard(t, r)
	assertCodexCard(t, card, r.Status.LatestReset.Source, r.Status.ActiveWatch.Source)
	if len(card.Body.Elements) != 4 || !strings.Contains(allContent(card), "**AI 预测**") {
		t.Fatal("watch-only report grouping incorrect")
	}
}

func durationPointer(v time.Duration) *time.Duration { return &v }
func intPointer(v int) *int                          { return &v }

func TestCodexDailyExternalTextsAreBoundedPlainText(t *testing.T) {
	r := exampleCodexReport()
	r.Status.LatestReset.Text = "latest **literal** <at id=all>all</at>"
	r.Status.ScheduledReset.Text = "scheduled [literal](https://evil.invalid) <at id=user>user</at>"
	r.Status.ActiveWatch.Text = "watch <at ids=a,b></at> <person id=x>literal</person>"
	r.Status.ActiveWatch.ForecastWindow = "window <at id=all></at> **literal**"
	body, err := FormatCodexReport(r)
	if err != nil {
		t.Fatal(err)
	}
	d := destination()
	d.Prefix = "prefix <at id=all></at>"
	payload, _ := capturedCard(t, body, d)
	assertCodexCard(t, payload.Card, r.Status.LatestReset.Source, r.Status.ScheduledReset.Source, r.Status.ActiveWatch.Source)
	for _, text := range []string{r.Status.LatestReset.Text, r.Status.ScheduledReset.Text, r.Status.ActiveWatch.Text, "预测窗口：" + r.Status.ActiveWatch.ForecastWindow} {
		found := false
		walkElements(payload.Card.Body.Elements, func(node map[string]any) {
			if node["content"] == neutralizeMentions(text) {
				found = true
				if node["tag"] != "plain_text" || node["lines"] != float64(2) {
					t.Fatal("external API text should be a two-line plain_text snippet")
				}
			}
		})
		if !found {
			t.Fatal("external text was lost or treated as markup")
		}
	}
	if !strings.HasPrefix(payload.Card.Header.Title.Content, neutralizeMentions(d.Prefix)) {
		t.Fatal("prefix mention guard lost")
	}
}

func TestCodexCardsBoundFinalSignedWireBytesAndPreserveFullRecords(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("abc", 30000), strings.Repeat("中文é", 10000),
		strings.Repeat("\x00\x01\"\\\n<>&\u2028\u2029", 5000), strings.Repeat("<at id=all>", 10000),
	} {
		r := exampleCodexReset()
		r.Text = text
		body, err := FormatCodexReset(r, true)
		if err != nil {
			t.Fatal(err)
		}
		var saved cardRecord
		if json.Unmarshal([]byte(body), &saved) != nil || saved.CodexReset.Text != text {
			t.Fatal("durable event text was truncated")
		}
		d := destination()
		d.Prefix = strings.Repeat("\x00", maxPrefixBytes)
		payload, wire := capturedCard(t, body, d)
		assertCodexCard(t, payload.Card, r.Source)
		if len(wire) > maxCardRequestBytes || payload.Sign != sign(d.Secret, payload.Timestamp) || !strings.Contains(allContent(payload.Card), "上游原文过长，已截断") {
			t.Fatal("event size/signature/truncation invalid")
		}
		found := false
		walkElements(payload.Card.Body.Elements, func(node map[string]any) {
			if node["tag"] == "plain_text" && node["lines"] == float64(4) {
				found = true
				snippet := strings.ReplaceAll(node["content"].(string), "<\u200bat", "<at")
				if !utf8.ValidString(snippet) || !strings.HasPrefix(text, snippet) {
					t.Fatal("event truncation corrupted original UTF-8")
				}
			}
		})
		if !found {
			t.Fatal("event lost its original-text excerpt")
		}
	}

	r := exampleCodexReport()
	text := strings.Repeat("<at\x00\u2028中文", 30000)
	r.Status.LatestReset.Text, r.Status.ScheduledReset.Text, r.Status.ActiveWatch.Text = text, text, text
	r.Status.ActiveWatch.ForecastWindow = strings.Repeat("<at\x00", 200)
	// Long, JSON-escaped source URLs leave a tight but feasible text budget.
	for _, source := range []*model.CodexSource{&r.Status.LatestReset.Source, &r.Status.ScheduledReset.Source, &r.Status.ActiveWatch.Source} {
		source.URL = "https://codex-resets.com/?q=" + strings.Repeat("&", 500)
	}
	body, err := FormatCodexReport(r)
	if err != nil {
		t.Fatal(err)
	}
	var saved cardRecord
	if json.Unmarshal([]byte(body), &saved) != nil || saved.CodexReport.Status.LatestReset.Text != text || saved.CodexReport.Status.ScheduledReset.Text != text || saved.CodexReport.Status.ActiveWatch.Text != text {
		t.Fatal("durable daily texts were truncated")
	}
	d := destination()
	d.Prefix = strings.Repeat("\x00", maxPrefixBytes)
	full, _ := json.Marshal(cardPayload{MsgType: "interactive", Timestamp: "1599360473", Sign: sign(d.Secret, "1599360473"), Card: renderCodexReport(r, d.Prefix, codexSnippetRunes)})
	if len(full) <= maxCardRequestBytes {
		t.Fatalf("stress fixture must exercise the final-byte budget, got %d", len(full))
	}
	payload, wire := capturedCard(t, body, d)
	assertCodexCard(t, payload.Card, r.Status.LatestReset.Source, r.Status.ScheduledReset.Source, r.Status.ActiveWatch.Source)
	if len(wire) > maxCardRequestBytes || payload.Sign != sign(d.Secret, payload.Timestamp) {
		t.Fatal("daily signed request exceeded its byte limit")
	}
	excerpts := 0
	walkElements(payload.Card.Body.Elements, func(node map[string]any) {
		if node["tag"] == "plain_text" && node["lines"] == float64(2) {
			snippet := strings.ReplaceAll(node["content"].(string), "<\u200bat", "<at")
			if strings.HasPrefix(snippet, "预测窗口：") {
				return
			}
			excerpts++
			if !utf8.ValidString(snippet) || utf8.RuneCountInString(snippet) >= codexSnippetRunes || !strings.HasSuffix(snippet, "…") || !strings.HasPrefix(text, strings.TrimSuffix(snippet, "…")) {
				t.Fatal("daily snippet was not safely reduced to the available budget")
			}
		}
	})
	if excerpts != 3 {
		t.Fatal("daily source snippets should be retained within the budget")
	}
}

func TestCodexOversizeMetadataFailsBeforeHTTP(t *testing.T) {
	r := exampleCodexReport()
	for _, source := range []*model.CodexSource{&r.Status.LatestReset.Source, &r.Status.ScheduledReset.Source, &r.Status.ActiveWatch.Source} {
		source.URL = "https://codex-resets.com/?q=" + strings.Repeat("&", 1900)
	}
	body, err := FormatCodexReport(r)
	if err != nil {
		t.Fatal(err)
	}
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversize report reached the transport")
		return nil, nil
	})
	sendError(t, c.Send(context.Background(), destination(), body), "payload_too_large", true)
}

func TestCodexInvalidFormatsAndMixedDurableRecords(t *testing.T) {
	for _, mutate := range []func(*model.CodexReset){
		func(r *model.CodexReset) { r.ID = "private\nbad-id" },
		func(r *model.CodexReset) { r.ResetType = "scheduled" },
		func(r *model.CodexReset) { r.AnnouncedAt = time.Time{} },
		func(r *model.CodexReset) { r.Text = "\xffprivate-text" },
		func(r *model.CodexReset) { r.Text = strings.Repeat("x", (1<<20)+1) },
		func(r *model.CodexReset) { r.Source.Type = "unknown" },
		func(r *model.CodexReset) { r.Source.URL = "https://private.invalid/" },
		func(r *model.CodexReset) { r.Source.URL = "http://x.com/thsottiaux/status/1" },
		func(r *model.CodexReset) { r.Source.Author = "fake" },
	} {
		r := exampleCodexReset()
		mutate(&r)
		if body, err := FormatCodexReset(r, false); body != "" || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid event format did not fail safely", err)
		}
	}
	for _, mutate := range []func(*model.CodexReport){
		func(r *model.CodexReport) { r.AsOf = time.Time{} },
		func(r *model.CodexReport) { r.Timezone = "private/invalid-zone" },
		func(r *model.CodexReport) { r.Date = "2026-07-11" },
		func(r *model.CodexReport) { r.Status.GeneratedAt = time.Time{} },
		func(r *model.CodexReport) { r.Status.Stats.Total = -1 },
		func(r *model.CodexReport) { *r.Status.Stats.AvgIntervalDays = math.NaN() },
		func(r *model.CodexReport) { *r.LongestIntervalDays = -1 },
		func(r *model.CodexReport) { r.Status.LatestReset.ID = "" },
		func(r *model.CodexReport) { r.Status.ScheduledReset.Status = "completed" },
		func(r *model.CodexReport) { *r.Status.ScheduledReset.ScheduledFor = time.Time{} },
		func(r *model.CodexReport) { r.Status.ActiveWatch.ResetChancePercent = intPointer(101) },
		func(r *model.CodexReport) { r.Status.ActiveWatch.ExpiresAt = r.Status.ActiveWatch.ObservedAt },
	} {
		r := exampleCodexReport()
		mutate(&r)
		if body, err := FormatCodexReport(r); body != "" || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid report format did not fail safely", err)
		}
	}
	post, reset, report := examplePost(), exampleCodexReset(), exampleCodexReport()
	invalidReport := exampleCodexReport()
	invalidReport.Status.ScheduledReset.Status = "completed"
	for _, record := range []cardRecord{
		{Version: cardRecordVersion, Post: &post, CodexReset: &reset},
		{Version: cardRecordVersion, Notice: "private-notice", CodexReset: &reset},
		{Version: cardRecordVersion, Reset: &model.ResetAssessment{}, CodexReset: &reset},
		{Version: cardRecordVersion, CodexReport: &report, CodexReset: &reset},
		{Version: cardRecordVersion, Post: &post, CodexReport: &report},
		{Version: cardRecordVersion, Notice: "private-notice", CodexReport: &report},
		{Version: cardRecordVersion, Reset: &model.ResetAssessment{}, CodexReport: &report},
		{Version: cardRecordVersion, CodexReport: &report, Debug: true},
		{Version: cardRecordVersion, CodexReset: &model.CodexReset{}},
		{Version: cardRecordVersion, CodexReport: &invalidReport},
		{Version: cardRecordVersion, Post: &post, Notice: "private-notice"},
		{Version: cardRecordVersion, Reset: &model.ResetAssessment{}},
	} {
		if body, err := encodeCard(record); body != "" || err == nil {
			t.Fatal("invalid or mixed content accepted by encoder")
		}
		wire, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		c := New(time.Second)
		c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid typed record reached transport")
			return nil, nil
		})
		sendError(t, c.Send(context.Background(), destination(), string(wire)), "invalid_request", true)
		if CardPost(string(wire)) != nil {
			t.Fatal("invalid mixed record exposed post evidence")
		}
	}
	for _, raw := range []string{`{"tibo_card":"tibo.post-card.v1","codex_reset":42}`, `{"tibo_card":"tibo.post-card.v1","codex_report":{}}`} {
		sendError(t, New(time.Second).Send(context.Background(), destination(), raw), "invalid_request", true)
	}
	// These older records remain notices, including a deliberately empty notice.
	for _, text := range []string{"", "old test notice"} {
		body, err := FormatNotice(text)
		if err != nil {
			t.Fatal(err)
		}
		capturedCard(t, body, destination())
	}
	capturedCard(t, legacyDebugPrefix+" old queued debug", destination())
}

func TestCodexSnippetRuneBoundaries(t *testing.T) {
	for _, limit := range []int{0, 1, 2, codexSnippetRunes} {
		for _, n := range []int{0, 1, 2, 179, 180, 181, 1000} {
			text := strings.Repeat("界", n)
			got := codexSnippet(text, limit)
			if !utf8.ValidString(got) || utf8.RuneCountInString(got) > limit || (n <= limit && got != text) {
				t.Fatal("invalid daily snippet boundary")
			}
		}
	}
}
