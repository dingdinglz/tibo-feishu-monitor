package feishu

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"tibo-monitor/internal/model"
)

const (
	codexSnippetRunes = 180
	codexAttribution  = "[数据来源：Codex Resets](" + model.CodexResetsURL + ")"
)

// FormatCodexReset preserves an upstream event as a typed durable record. It is
// not post evidence and must not acquire a TypeSafe assessment during delivery.
func FormatCodexReset(r model.CodexReset, debug bool) (string, error) {
	if !r.Valid() {
		return "", errors.New("feishu: format_invalid_codex_reset")
	}
	return encodeCard(cardRecord{Version: cardRecordVersion, CodexReset: &r, Debug: debug})
}

// FormatCodexWatch preserves a possible reset, never a confirmed reset or a
// TypeSafe assessment. Retries retain the original observation and expiry.
func FormatCodexWatch(w model.CodexWatch) (string, error) {
	if !w.Valid() {
		return "", errors.New("feishu: format_invalid_codex_watch")
	}
	return encodeCard(cardRecord{Version: cardRecordVersion, CodexWatch: &w})
}

// FormatCodexReport persists the entire snapshot, including its observation and
// upstream generation times. Retries refresh credentials, never the report.
func FormatCodexReport(r model.CodexReport) (string, error) {
	if !r.Valid() {
		return "", errors.New("feishu: format_invalid_codex_report")
	}
	return encodeCard(cardRecord{Version: cardRecordVersion, CodexReport: &r})
}

func codexCard(prefix, title, subtitle string) cardView {
	var card cardView
	card.Schema = "2.0"
	card.Config.WidthMode = "default"
	card.Header.Template = "yellow"
	card.Header.Title = plain(prefix + " · " + title)
	sub := plain(subtitle)
	card.Header.Subtitle = &sub
	card.Header.Icon = map[string]string{"tag": "standard_icon", "token": "calendar_colorful"}
	card.Body.Padding = "12px"
	card.Body.VerticalSpacing = "12px"
	return card
}

func codexResetState(resetType string) string {
	if resetType == "banked" {
		return "已发放备用重置额度"
	}
	return "已执行重置"
}

func codexSourceLabel(s model.CodexSource) string {
	if s.Type == "observed" {
		return "网站观测"
	}
	return "X 公告 · @" + s.Author
}

func codexTimestamp(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04:05 -07:00")
}

func codexHero(text string) map[string]any {
	t := plain(text)
	t.TextSize, t.TextColor = "heading-2", "orange"
	return map[string]any{"tag": "div", "text": t}
}

func codexText(text string, lines int) map[string]any {
	t := plain(text)
	t.Lines = lines
	return map[string]any{"tag": "div", "text": t}
}

func codexAttributionElement() map[string]any {
	// This is the only link rendered as Markdown: both label and URL are fixed.
	// External source URLs go through CodexSource.Link and native open_url only.
	return map[string]any{"tag": "markdown", "content": codexAttribution, "text_size": "notation"}
}

func renderCodexReset(r model.CodexReset, debug bool, prefix, body string, truncated bool) cardView {
	card := codexCard(prefix, "Codex 重置通知", codexSourceLabel(r.Source)+" · "+codexTimestamp(r.AnnouncedAt, beijing)+"（北京时间）")
	hero := []any{codexHero(codexResetState(r.ResetType)), secondary("据 Codex Resets 上游记录；不代表个人账户额度状态。")}
	if debug {
		card.Header.Title = plain(prefix + " · DEBUG 测试 · Codex 重置通知")
		card.Header.Template = "orange"
		hero = append(hero, secondary("DEBUG 测试：重发已保存的上游记录，不是新增通知。"))
	}
	card.Body.Elements = append(card.Body.Elements, group("重置状态", "yellow-50", hero...))
	if body == "" {
		body = "上游未提供文字说明。"
		if truncated {
			body = "上游正文已省略，请查看来源。"
		}
	}
	original := []any{codexText(body, 4)}
	if truncated {
		original = append(original, secondary("上游原文过长，已截断；请查看来源阅读完整内容。"))
	}
	original = append(original, secondary("记录 ID："+r.ID), openButton("查看来源", r.Source.Link(), false))
	card.Body.Elements = append(card.Body.Elements,
		group("上游记录", "grey-50", original...),
		group("数据来源", "default", secondary("公告时间："+codexTimestamp(r.AnnouncedAt, beijing)+"；重试保留原记录时间。"), codexAttributionElement()),
	)
	return card
}

func codexWatchLevel(w model.CodexWatch) string {
	if w.Level == "strong" {
		return "较强信号"
	}
	return "关注信号"
}

func codexWatchProbability(w model.CodexWatch) string {
	if w.ResetChancePercent == nil {
		return "未知"
	}
	return strconv.Itoa(*w.ResetChancePercent) + "%"
}

func renderCodexWatch(w model.CodexWatch, prefix, body string, truncated bool) cardView {
	card := codexCard(prefix, "Codex 可能重置提醒", "AI 预测 · "+codexWatchLevel(w)+" · "+codexTimestamp(w.ObservedAt, beijing)+"（北京时间）")
	card.Header.Template = "orange"
	prediction := []any{codexHero("可能重置 · 重置概率：" + codexWatchProbability(w))}
	if window := codexSnippet(w.ForecastWindow, 60); window != "" {
		prediction = append(prediction, codexText("预测窗口："+window, 2))
	}
	prediction = append(prediction,
		secondary("观测时间："+codexTimestamp(w.ObservedAt, beijing)),
		secondary("有效期至："+codexTimestamp(w.ExpiresAt, beijing)),
		secondary("第三方 AI 预测，非官方承诺；不表示已执行重置。"),
	)
	if body == "" {
		body = "上游未提供文字说明。"
		if truncated {
			body = "上游正文已省略，请查看来源。"
		}
	}
	original := []any{codexText(body, 4)}
	if truncated {
		original = append(original, secondary("上游原文过长，已截断；请查看来源阅读完整内容。"))
	}
	original = append(original, secondary(codexSourceLabel(w.Source)), openButton("查看预测来源", w.Source.Link(), false))
	card.Body.Elements = append(card.Body.Elements,
		group("可能重置 / AI 预测", "orange-50", prediction...),
		group("预测来源", "grey-50", original...),
		group("数据来源", "default", secondary("固定预测快照；重试保留原观测时间及有效期。"), codexAttributionElement()),
	)
	return card
}

func renderCodexReport(r model.CodexReport, prefix string, snippetRunes int) cardView {
	// Validated at the durable boundary. Do not consult the delivery clock here.
	loc, _ := time.LoadLocation(r.Timezone)
	card := codexCard(prefix, "Codex 重置日报", r.Date+" · "+r.Timezone)
	latest := r.Status.LatestReset
	lastAt := r.Status.Stats.LastResetAt
	if latest != nil {
		lastAt = &latest.AnnouncedAt
	}
	hero := []any{codexHero("距最近重置 " + codexElapsed(r.AsOf, lastAt))}
	if lastAt != nil {
		hero = append(hero, secondary("最近重置："+codexTimestamp(*lastAt, loc)))
		if lastAt.After(r.AsOf) {
			hero = append(hero, secondary("记录时间晚于本次快照，暂无法计算间隔。"))
		}
	}
	if latest != nil {
		hero = append(hero, secondary(codexResetState(latest.ResetType)+" · "+codexSourceLabel(latest.Source)+" · 据上游记录"))
		hero = appendCodexSnippet(hero, latest.Text, snippetRunes)
		hero = append(hero, openButton("查看最近重置来源", latest.Source.Link(), false))
	} else {
		hero = append(hero, secondary("上游暂无最近重置详情。"))
	}
	card.Body.Elements = append(card.Body.Elements, group("最近重置", "yellow-50", hero...), codexStatTiles(r))

	var upcoming []any
	upcomingLabel := "待执行计划"
	if scheduled := r.Status.ScheduledReset; scheduled != nil {
		kind := "常规重置"
		if scheduled.ResetType == "banked" {
			kind = "备用重置额度发放"
		}
		upcoming = append(upcoming, paragraph("待执行 · "+kind))
		if scheduled.ScheduledFor == nil {
			upcoming = append(upcoming, secondary("计划时间：待定；尚未确认执行。"))
		} else {
			upcoming = append(upcoming, secondary("计划时间："+codexTimestamp(*scheduled.ScheduledFor, loc)))
			if !scheduled.ScheduledFor.After(r.AsOf) {
				upcoming = append(upcoming, secondary("计划时间已过，尚未确认执行。"))
			} else {
				upcoming = append(upcoming, secondary("以上为上游计划，尚未确认执行。"))
			}
		}
		upcoming = append(upcoming, secondary(codexSourceLabel(scheduled.Source)+" · 公告："+codexTimestamp(scheduled.AnnouncedAt, loc)))
		upcoming = appendCodexSnippet(upcoming, scheduled.Text, snippetRunes)
		upcoming = append(upcoming, openButton("查看计划来源", scheduled.Source.Link(), false))
	}
	if watch := r.Status.ActiveWatch; watch != nil && watch.ExpiresAt.After(r.AsOf) {
		upcomingLabel = "AI 预测"
		if len(upcoming) > 0 {
			upcomingLabel = "待执行 / AI 预测"
		}
		upcoming = append(upcoming, paragraph("AI 预测 · "+codexWatchLevel(*watch)+" · 重置概率："+codexWatchProbability(*watch)))
		if window := codexSnippet(watch.ForecastWindow, min(snippetRunes, 60)); window != "" {
			upcoming = append(upcoming, codexText("预测窗口："+window, 2))
		}
		upcoming = append(upcoming,
			secondary(codexSourceLabel(watch.Source)+" · 观测："+codexTimestamp(watch.ObservedAt, loc)),
			secondary("有效期至："+codexTimestamp(watch.ExpiresAt, loc)),
			secondary("AI 预测，非官方承诺；不表示已执行重置。"),
		)
		upcoming = appendCodexSnippet(upcoming, watch.Text, snippetRunes)
		upcoming = append(upcoming, openButton("查看预测来源", watch.Source.Link(), false))
	}
	if len(upcoming) > 0 {
		card.Body.Elements = append(card.Body.Elements, group(upcomingLabel, "grey-50", upcoming...))
	}
	card.Body.Elements = append(card.Body.Elements, group("快照时间", "default",
		secondary("抓取 / 截至："+codexTimestamp(r.AsOf, loc)),
		secondary("上游生成："+codexTimestamp(r.Status.GeneratedAt, loc)),
		secondary("固定快照；重试不更新以上时间或间隔。"), codexAttributionElement(),
	))
	return card
}

func codexStatTiles(r model.CodexReport) map[string]any {
	var columns []any
	for _, stat := range []struct{ label, value string }{
		{"重置次数", strconv.Itoa(r.Status.Stats.Total) + "次"},
		{"平均间隔", codexDays(r.Status.Stats.AvgIntervalDays)},
		{"最长等待", codexDays(r.LongestIntervalDays)},
	} {
		// Smaller, neutral stats support the single yellow elapsed-time hero.
		value := plain(stat.value)
		value.TextSize, value.TextAlign = "normal", "center"
		label := plain(stat.label)
		label.TextSize, label.TextColor, label.TextAlign = "notation", "grey", "center"
		columns = append(columns, map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "background_style": "grey-50",
			"padding": "12px", "vertical_spacing": "4px", "elements": []any{
				map[string]any{"tag": "div", "text": value}, map[string]any{"tag": "div", "text": label},
			},
		})
	}
	return map[string]any{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": columns}
}

func codexDays(days *float64) string {
	if days == nil {
		return "暂无数据"
	}
	return strings.TrimSuffix(strconv.FormatFloat(math.Abs(*days), 'f', 1, 64), ".0") + "天"
}

func codexElapsed(asOf time.Time, lastAt *time.Time) string {
	if lastAt == nil || lastAt.After(asOf) {
		return "暂无数据"
	}
	elapsed := asOf.Sub(*lastAt)
	if elapsed < time.Hour {
		return "不足1小时"
	}
	if elapsed < 24*time.Hour {
		return strconv.FormatInt(int64(elapsed/time.Hour), 10) + "小时"
	}
	days := elapsed.Hours() / 24
	return codexDays(&days)
}

func appendCodexSnippet(elements []any, text string, limit int) []any {
	if snippet := codexSnippet(text, limit); snippet != "" {
		return append(elements, codexText(snippet, 2))
	}
	return elements
}

// Keep the ellipsis within the rune budget and never split UTF-8. The full
// upstream text remains in the durable record, even when a snippet is omitted.
func codexSnippet(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	count, last := 0, 0
	for i := range text {
		if count == limit {
			return text[:last] + "…"
		}
		last = i
		count++
	}
	return text
}
