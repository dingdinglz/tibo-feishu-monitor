package feishu

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"tibo-monitor/internal/model"
)

// Leave room below the custom-bot 20 KB limit, including the current prefix,
// signature and timestamp. Never turn a single post into multiple cards.
const maxCardRequestBytes = 19000

var beijing, beijingErr = time.LoadLocation("Asia/Shanghai")

type cardText struct {
	Tag       string `json:"tag"`
	Content   string `json:"content"`
	TextSize  string `json:"text_size,omitempty"`
	TextColor string `json:"text_color,omitempty"`
	TextAlign string `json:"text_align,omitempty"`
	Lines     int    `json:"lines,omitempty"`
}

type cardView struct {
	Schema string `json:"schema"`
	Config struct {
		WidthMode string `json:"width_mode"`
	} `json:"config"`
	Header struct {
		Template string            `json:"template"`
		Title    cardText          `json:"title"`
		Subtitle *cardText         `json:"subtitle,omitempty"`
		Icon     map[string]string `json:"icon,omitempty"`
	} `json:"header"`
	Body struct {
		Padding         string `json:"padding"`
		VerticalSpacing string `json:"vertical_spacing"`
		Elements        []any  `json:"elements"`
	} `json:"body"`
}

type cardPayload struct {
	Timestamp string   `json:"timestamp,omitempty"`
	Sign      string   `json:"sign,omitempty"`
	MsgType   string   `json:"msg_type"`
	Card      cardView `json:"card"` // Webhooks take an OBJECT, unlike application IM content.
}

func plain(text string) cardText {
	return cardText{Tag: "plain_text", Content: neutralizeMentions(text)}
}
func paragraph(text string) map[string]any { return map[string]any{"tag": "div", "text": plain(text)} }
func secondary(text string) map[string]any {
	t := plain(text)
	t.TextSize = "notation"
	t.TextColor = "grey"
	return map[string]any{"tag": "div", "text": t}
}

func group(label, background string, elements ...any) map[string]any {
	// Markdown is used only for these fixed, trusted labels. All external
	// text remains plain_text: no mention, Markdown or callback injection.
	heading := map[string]any{"tag": "markdown", "content": "**" + label + "**"}
	return map[string]any{
		"tag": "column_set", "flex_mode": "none", "columns": []any{map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "background_style": background,
			"padding": "12px", "vertical_spacing": "4px", "elements": append([]any{heading}, elements...),
		}},
	}
}

func openButton(label, url string, primary bool) map[string]any {
	b := map[string]any{"tag": "button", "text": plain(label), "behaviors": []any{map[string]any{"type": "open_url", "default_url": url}}}
	if primary {
		b["type"] = "primary_filled"
		b["width"] = "fill"
	}
	return b
}

func contextGroup(label string, c model.Context) map[string]any {
	text := "上下文暂不可用"
	if c.Available {
		author := c.Author
		if author == "" {
			author = "unknown"
		}
		text = "@" + author + "\n" + contextSnippet(c.Text)
	}
	return group(label, "grey-50", paragraph(text), openButton("查看上下文", postURL(c.ID, c.Author), false))
}

func resetGroup(a *model.ResetAssessment, color string) map[string]any {
	if a == nil || !a.Valid() {
		return group("本次额度重置概率", "grey-50", paragraph("暂不可用"),
			secondary("TypeSafe 未返回有效概率；未使用模拟值。"))
	}
	probability := math.Abs(*a.Probability) // normalize a valid IEEE negative zero
	filled := int(math.Round(probability * 10))
	percent := strings.TrimSuffix(strconv.FormatFloat(probability*100, 'f', 1, 64), ".0")
	bar := plain(strings.Repeat("█", filled) + strings.Repeat("░", 10-filled) + "  " + percent + "%")
	bar.TextSize = "heading-3"
	bar.TextColor = color
	return group("本次额度重置概率", color+"-50", map[string]any{"tag": "div", "text": bar},
		secondary(fmt.Sprintf("TypeSafe · %s · 参考此前 %d 条动态", a.Model, a.HistoryCount)),
		secondary("AI 估计本条动态是否预告或确认 Codex 额度重置，并非重置保证。"))
}

func renderCard(record cardRecord, prefix, body string, truncated bool) cardView {
	if record.CodexReset != nil {
		return renderCodexReset(*record.CodexReset, record.Debug, prefix, body, truncated)
	}
	var card cardView
	card.Schema = "2.0"
	card.Config.WidthMode = "default"
	card.Body.Padding = "12px"
	card.Body.VerticalSpacing = "12px"
	label, color := "测试通知", "blue"
	if p := record.Post; p != nil {
		label = "动态"
		switch p.Kind {
		case "post":
			label = "原创"
		case "reply":
			label = "回复"
			color = "purple"
		case "quote":
			label = "引用"
			color = "turquoise"
		}
		author := p.Author
		if author == "" {
			author = "unknown"
		}
		date := "时间未知"
		if !p.CreatedAt.IsZero() {
			date = p.CreatedAt.In(beijing).Format("2006-01-02 15:04:05") + "（北京时间）"
		}
		subtitle := plain("@" + author + " · " + date)
		card.Header.Subtitle = &subtitle
	}
	if record.Debug {
		label = "DEBUG 测试 · " + label
		color = "orange"
	}
	card.Header.Template = color
	card.Header.Title = plain(prefix + " · " + label)
	var original []any
	if record.Debug {
		original = append(original, secondary("本轮未发现新增，重发最新一条；这不是新增通知。"))
	}
	if body == "" {
		body = "（无文字内容，详情请查看原帖）"
	}
	original = append(original, paragraph(body))
	if truncated {
		note := "内容过长，已截断。"
		if record.Post != nil {
			note = "原文过长，已截断；请点击“查看原帖”阅读完整内容。"
		}
		original = append(original, secondary(note))
	}
	if p := record.Post; p != nil {
		original = append(original, secondary("ID："+p.ID))
		card.Body.Elements = append(card.Body.Elements, resetGroup(record.Reset, color), group("原文", color+"-50", original...))
		if p.Parent != nil {
			card.Body.Elements = append(card.Body.Elements, contextGroup("回复上下文", *p.Parent))
		}
		if p.Quote != nil {
			card.Body.Elements = append(card.Body.Elements, contextGroup("引用内容", *p.Quote))
		}
		card.Body.Elements = append(card.Body.Elements, openButton("查看原帖", postURL(p.ID, p.Author), true))
	} else {
		card.Body.Elements = append(card.Body.Elements, group("通知内容", color+"-50", original...), secondary("Tibo 监控 · 测试卡片"))
	}
	return card
}

func (c *Client) cardBody(prefix, secret string, record cardRecord) ([]byte, error) {
	if beijingErr != nil {
		return nil, &SendError{Code: "invalid_request", Permanent: true}
	}
	payload := cardPayload{MsgType: "interactive"}
	if secret != "" {
		payload.Timestamp = strconv.FormatInt(c.now().Unix(), 10)
		payload.Sign = sign(secret, payload.Timestamp)
	}
	var render func(int) cardView
	var limit int
	if report := record.CodexReport; report != nil {
		// Daily optional texts are snippets, not unbounded copies of the API.
		// Reduce them together if escaping/prefix/signing exhausts the budget.
		limit = codexSnippetRunes
		render = func(n int) cardView { return renderCodexReport(*report, prefix, n) }
	} else {
		text := record.Notice
		if record.Post != nil {
			text = record.Post.Text
		} else if record.CodexReset != nil {
			text = record.CodexReset.Text
		}
		runes := []rune(text)
		limit = len(runes)
		render = func(n int) cardView { return renderCard(record, prefix, string(runes[:n]), n < limit) }
	}
	encode := func(n int) ([]byte, error) {
		payload.Card = render(n)
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, &SendError{Code: "payload_encoding", Permanent: true}
		}
		return raw, nil
	}
	full, err := encode(limit)
	if err != nil {
		return nil, err
	}
	if len(full) <= maxCardRequestBytes {
		return full, nil
	}
	// Binary search on rune boundaries using the ACTUAL final JSON bytes. This
	// includes HTML/control escaping, mention guards and the truncation notice.
	best, err := encode(0)
	if err != nil {
		return nil, err
	}
	if len(best) > maxCardRequestBytes {
		return nil, &SendError{Code: "payload_too_large", Permanent: true}
	}
	low, high := 0, limit
	for low < high {
		mid := (low + high + 1) / 2
		candidate, err := encode(mid)
		if err != nil {
			return nil, err
		}
		if len(candidate) <= maxCardRequestBytes {
			low = mid
			best = candidate
		} else {
			high = mid - 1
		}
	}
	return best, nil
}
