package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/model"
)

func examplePost() model.Post {
	return model.Post{ID: "1953337039510003712", Author: "thsottiaux", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), URL: "https://x.com/thsottiaux/status/1953337039510003712", Kind: "post"}
}

func capturedCard(t *testing.T, body string, d config.Destination) (cardPayload, []byte) {
	t.Helper()
	client := New(time.Second)
	var payload cardPayload
	var wire []byte
	calls := 0
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var err error
		wire, err = io.ReadAll(r.Body)
		if err != nil || len(wire) > maxCardRequestBytes || int64(len(wire)) != r.ContentLength {
			t.Fatal("invalid card wire length", len(wire), err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["msg_type"]) != `"interactive"` || len(fields["card"]) == 0 || fields["card"][0] != '{' {
			t.Fatal("webhook card must be an object with interactive msg_type")
		}
		if _, ok := fields["content"]; ok {
			t.Fatal("card used text/application IM payload")
		}
		if err := json.Unmarshal(wire, &payload); err != nil {
			t.Fatal(err)
		}
		return testResponse(200, `{"code":0}`), nil
	})
	if err := client.Send(context.Background(), d, body); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("one item must send exactly one HTTP card", calls)
	}
	if payload.Card.Schema != "2.0" || payload.Card.Config.WidthMode != "default" {
		t.Fatal("wrong card version/layout")
	}
	return payload, wire
}

func walkElements(elements []any, fn func(map[string]any)) {
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			fn(x)
			for _, child := range x {
				visit(child)
			}
		case []any:
			for _, child := range x {
				visit(child)
			}
		}
	}
	visit(elements)
}

func allContent(card cardView) string {
	var text strings.Builder
	text.WriteString(card.Header.Title.Content)
	if card.Header.Subtitle != nil {
		text.WriteString(card.Header.Subtitle.Content)
	}
	walkElements(card.Body.Elements, func(node map[string]any) {
		if content, ok := node["content"].(string); ok {
			text.WriteString(content)
			text.WriteByte('\n')
		}
	})
	return text.String()
}

func originalText(card cardView) string {
	first := card.Body.Elements[0].(map[string]any)
	column := first["columns"].([]any)[0].(map[string]any)
	for _, element := range column["elements"].([]any) {
		obj := element.(map[string]any)
		if obj["tag"] != "div" {
			continue
		}
		text := obj["text"].(map[string]any)
		if _, secondary := text["text_size"]; !secondary {
			return text["content"].(string)
		}
	}
	return ""
}

func assertOriginalButton(t *testing.T, card cardView, want string) {
	t.Helper()
	found := 0
	walkElements(card.Body.Elements, func(node map[string]any) {
		if node["tag"] != "button" {
			return
		}
		behaviors := node["behaviors"].([]any)
		if len(behaviors) != 1 || behaviors[0].(map[string]any)["type"] != "open_url" {
			t.Fatal("button should not trigger callbacks")
		}
		text := node["text"].(map[string]any)["content"]
		if text == "查看原帖" {
			found++
			if behaviors[0].(map[string]any)["default_url"] != want {
				t.Fatal("wrong post link")
			}
		}
	})
	if found != 1 {
		t.Fatal("must retain exactly one original-post button", found)
	}
}

func TestEveryPostIsOneCardEvenForLongEscapedUnicode(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""}, {"short", "原文🙂 **not markdown** <at id=all>all</at>"},
		{"ascii", strings.Repeat("abc", 50000)},
		{"unicode", strings.Repeat("中文🙂🚀é", 10000)},
		{"escaping", strings.Repeat("\\\"\x00\x01\b\f\n\r\t<>&\u2028\u2029", 4000)},
		{"mentions", strings.Repeat("<at", 20000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := examplePost()
			p.Text = tc.text
			messages, err := Format(p)
			if err != nil || len(messages) != 1 {
				t.Fatal("post was split or lost", len(messages), err)
			}
			var saved cardRecord
			if err := json.Unmarshal([]byte(messages[0]), &saved); err != nil || saved.Post == nil || saved.Post.Text != p.Text {
				t.Fatal("durable original text was truncated", err)
			}
			d := destination()
			d.Prefix = strings.Repeat("\x00", 512)
			payload, _ := capturedCard(t, messages[0], d)
			if payload.Timestamp == "" || payload.Sign == "" {
				t.Fatal("signing overhead must be included")
			}
			body := originalText(payload.Card)
			if !utf8.ValidString(body) {
				t.Fatal("truncation split UTF-8")
			}
			if p.Text != "" {
				restored := strings.ReplaceAll(body, "<\u200bat", "<at")
				if !strings.HasPrefix(p.Text, restored) {
					t.Fatal("card did not retain an original-text prefix")
				}
				truncated := restored != p.Text
				if truncated != strings.Contains(allContent(payload.Card), "原文过长，已截断") {
					t.Fatal("truncation must be explicit")
				}
			}
			assertOriginalButton(t, payload.Card, p.URL)
		})
	}
}

func TestCardTypesContextsAndSafeContent(t *testing.T) {
	for _, tc := range []struct{ kind, label, color string }{{"post", "原创", "blue"}, {"reply", "回复", "purple"}, {"quote", "引用", "turquoise"}, {"unknown", "动态", "blue"}} {
		t.Run(tc.kind, func(t *testing.T) {
			p := examplePost()
			p.Kind = tc.kind
			p.Text = "**literal text** <at user_id=all>all</at>"
			p.Parent = &model.Context{ID: "10", Author: "parent", Text: strings.Repeat("上下文🙂", 200), Available: true}
			p.Quote = &model.Context{ID: "11", Author: "quote", Text: "must not expose invisible content", Available: false}
			messages, err := Format(p)
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := capturedCard(t, messages[0], destination())
			card := payload.Card
			if card.Header.Template != tc.color || !strings.Contains(card.Header.Title.Content, tc.label) {
				t.Fatal("wrong card type")
			}
			if card.Header.Subtitle == nil || card.Header.Subtitle.Content != "@thsottiaux · 2026-01-02 11:04:05（北京时间）" {
				t.Fatal("missing author or Beijing time")
			}
			contents := allContent(card)
			for _, want := range []string{"回复上下文", "@parent", contextSnippet(p.Parent.Text), "引用内容", "上下文暂不可用", "ID：" + p.ID} {
				if !strings.Contains(contents, want) {
					t.Fatal("missing card content", want)
				}
			}
			if strings.Contains(contents, p.Quote.Text) || strings.Contains(contents, p.Parent.Text) || strings.Contains(contents, "<at") {
				t.Fatal("unsafe or unbounded context")
			}
			if originalText(card) != neutralizeMentions(p.Text) {
				t.Fatal("plain original text changed")
			}
			walkElements(card.Body.Elements, func(node map[string]any) {
				if node["tag"] == "markdown" {
					content := node["content"]
					if content != "**原文**" && content != "**回复上下文**" && content != "**引用内容**" {
						t.Fatal("untrusted text used as markdown")
					}
				}
			})
			assertOriginalButton(t, card, p.URL)
		})
	}
}

func TestDebugAndTestSendUseCards(t *testing.T) {
	p := examplePost()
	p.Text = "A reply"
	debug, err := FormatDebug(p)
	if err != nil || len(debug) != 1 {
		t.Fatal(err)
	}
	payload, _ := capturedCard(t, debug[0], destination())
	if payload.Card.Header.Template != "orange" || !strings.Contains(allContent(payload.Card), "DEBUG 测试") || !strings.Contains(allContent(payload.Card), "不是新增通知") {
		t.Fatal("debug card not labeled")
	}
	assertOriginalButton(t, payload.Card, p.URL)
	notice, err := FormatNotice("explicit test card")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = capturedCard(t, notice, destination())
	if !strings.Contains(allContent(payload.Card), "explicit test card") {
		t.Fatal("missing test notice")
	}
	payload, _ = capturedCard(t, legacyDebugPrefix+" legacy queued text", destination())
	if payload.Card.Header.Template != "orange" {
		t.Fatal("legacy debug was not rendered as a card")
	}
}

func TestCardRetriesRefreshSignatureAndPrefix(t *testing.T) {
	p := examplePost()
	p.Text = "one post"
	body, err := Format(p)
	if err != nil {
		t.Fatal(err)
	}
	client := New(time.Second)
	now := time.Unix(1599360473, 0)
	client.now = func() time.Time { return now }
	var attempts []cardPayload
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload cardPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, payload)
		if len(attempts) == 1 {
			return testResponse(200, `{"code":11232}`), nil
		}
		return testResponse(200, `{"code":0}`), nil
	})
	d := destination()
	d.Secret = "demo"
	sendError(t, client.Send(context.Background(), d, body[0]), "11232", false)
	now = now.Add(time.Second)
	d.Prefix = "changed keyword"
	if err := client.Send(context.Background(), d, body[0]); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Sign != "l1N0gAcBjdwBvGm1xMjOF0XSyaLRpR7tuO5dHfhAYc8=" || attempts[0].Sign == attempts[1].Sign || !strings.HasPrefix(attempts[1].Card.Header.Title.Content, d.Prefix) {
		t.Fatal("card retries did not refresh credentials/prefix")
	}
}

func TestCardLinksAreCanonicalAndUnsignedCardOmitsCredentials(t *testing.T) {
	p := examplePost()
	p.URL = "https://evil.invalid/"
	p.Parent = &model.Context{ID: "456", Available: false}
	body, err := Format(p)
	if err != nil {
		t.Fatal(err)
	}
	d := destination()
	d.Secret = ""
	payload, wire := capturedCard(t, body[0], d)
	if payload.Sign != "" || payload.Timestamp != "" || strings.Contains(string(wire), `"sign"`) || strings.Contains(string(wire), "evil.invalid") {
		t.Fatal("unsigned/canonical card invalid")
	}
	assertOriginalButton(t, payload.Card, postURL(p.ID, p.Author))
}

func TestCardSafeErrorsAndContextBoundary(t *testing.T) {
	for _, mutate := range []func(*model.Post){
		func(p *model.Post) { p.ID = "" }, func(p *model.Post) { p.Text = "\xffprivate-text" },
		func(p *model.Post) { p.Author = "bad/author" }, func(p *model.Post) { p.URL = strings.Repeat("private-url", 2000) },
		func(p *model.Post) { p.Text = strings.Repeat("x", (1<<20)+1) },
		func(p *model.Post) { p.Parent = &model.Context{ID: "1", Text: "\xffprivate-context"} },
	} {
		p := examplePost()
		mutate(&p)
		body, err := Format(p)
		if err == nil || body != nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid data must fail safely", err)
		}
	}
	for _, n := range []int{0, 1, 299, 300, 301, 1000} {
		text := strings.Repeat("🙂", n)
		got := contextSnippet(text)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 300 || (n <= 300 && got != text) {
			t.Fatal("invalid context snippet")
		}
	}
	for _, raw := range []string{`{"tibo_card":"unknown"}`, `{"tibo_card":`, `{"tibo_card":"tibo.post-card.v1","post":{"id":"bad"}}`} {
		sendError(t, New(time.Second).Send(context.Background(), destination(), raw), "invalid_request", true)
	}
}
