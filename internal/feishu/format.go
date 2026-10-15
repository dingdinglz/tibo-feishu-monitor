package feishu

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"tibo-monitor/internal/model"
)

const (
	cardRecordVersion  = "tibo.post-card.v1"
	cardRecordPrefix   = `{"tibo_card":`
	maxCardRecordBytes = 8 << 20
	contextRunes       = 300
	legacyDebugPrefix  = "【DEBUG 测试】"
)

var decimalID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var loginName = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// Persist content, not credentials, keyword prefixes or signatures. Render the
// actual card on each attempt so changed prefixes and secrets also affect retries.
type cardRecord struct {
	Version     string                 `json:"tibo_card"`
	Post        *model.Post            `json:"post,omitempty"`
	Debug       bool                   `json:"debug,omitempty"`
	Notice      string                 `json:"notice,omitempty"`
	Reset       *model.ResetAssessment `json:"reset_assessment,omitempty"`
	CodexReset  *model.CodexReset      `json:"codex_reset,omitempty"`
	CodexReport *model.CodexReport     `json:"codex_report,omitempty"`
}

// CardPost returns the evidence for a post card, including debug replays. Text
// alerts, notices, Codex API snapshots and invalid records are not post evidence.
func CardPost(text string) *model.Post {
	record, _, err := decodeCard(text)
	if err != nil {
		return nil
	}
	return record.Post
}

func WithResetAssessment(text string, assessment model.ResetAssessment) (string, error) {
	record, isCard, err := decodeCard(text)
	if err != nil {
		return "", err
	}
	if !isCard || record.Post == nil || !assessment.Valid() {
		return "", errors.New("feishu: invalid_reset_assessment")
	}
	record.Reset = &assessment
	return encodeCard(record)
}

// Format always returns exactly one durable message per post. Oversize content
// is excerpted only in the rendered card; the original is retained in SQLite.
// The slice interface remains compatible with legacy outbox fragments.
func Format(p model.Post) ([]string, error)      { return formatPost(p, false) }
func FormatDebug(p model.Post) ([]string, error) { return formatPost(p, true) }

func formatPost(p model.Post, debug bool) ([]string, error) {
	if !validCardPost(&p) {
		return nil, errors.New("feishu: format_invalid_post")
	}
	// Only context snippets are displayed; bound the queue's extra footprint.
	if p.Parent != nil {
		c := *p.Parent
		c.Text = contextSnippet(c.Text)
		p.Parent = &c
	}
	if p.Quote != nil {
		c := *p.Quote
		c.Text = contextSnippet(c.Text)
		p.Quote = &c
	}
	body, err := encodeCard(cardRecord{Version: cardRecordVersion, Post: &p, Debug: debug})
	if err != nil {
		return nil, err
	}
	return []string{body}, nil
}

// FormatNotice is used by the explicit test-send command to test the same card
// transport as actual posts. Operational alerts continue to use plain text.
func FormatNotice(text string) (string, error) {
	if !utf8.ValidString(text) || len(text) > 1<<20 {
		return "", errors.New("feishu: format_invalid_notice")
	}
	return encodeCard(cardRecord{Version: cardRecordVersion, Notice: text})
}

func encodeCard(record cardRecord) (string, error) {
	if !validCardRecord(record) {
		return "", errors.New("feishu: format_invalid_card")
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > maxCardRecordBytes {
		return "", errors.New("feishu: format_invalid_card")
	}
	return string(raw), nil
}

func decodeCard(text string) (cardRecord, bool, error) {
	var record cardRecord
	if !strings.HasPrefix(text, cardRecordPrefix) {
		// An already queued test from the text-message version still gets a card.
		if strings.HasPrefix(text, legacyDebugPrefix) {
			return cardRecord{Version: cardRecordVersion, Notice: text, Debug: true}, true, nil
		}
		return record, false, nil
	}
	if len(text) > maxCardRecordBytes || !utf8.ValidString(text) || json.Unmarshal([]byte(text), &record) != nil || !validCardRecord(record) {
		return cardRecord{}, true, &SendError{Code: "invalid_request", Permanent: true}
	}
	return record, true, nil
}

func validCardRecord(record cardRecord) bool {
	if record.Version != cardRecordVersion || !utf8.ValidString(record.Notice) || len(record.Notice) > 1<<20 {
		return false
	}
	kinds := 0
	if record.Post != nil {
		if !validCardPost(record.Post) {
			return false
		}
		kinds++
	}
	if record.Notice != "" {
		kinds++
	}
	if record.CodexReset != nil {
		if !record.CodexReset.Valid() {
			return false
		}
		kinds++
	}
	if record.CodexReport != nil {
		if !record.CodexReport.Valid() || record.Debug {
			return false
		}
		kinds++
	}
	// Empty notices remain valid for old queues. Invalid post assessments still
	// render as unavailable, but an assessment never belongs to another kind.
	return kinds <= 1 && (record.Reset == nil || record.Post != nil)
}

func validCardPost(p *model.Post) bool {
	if !decimalID.MatchString(p.ID) || (p.Author != "" && !loginName.MatchString(p.Author)) {
		return false
	}
	for _, s := range []string{p.Text, p.URL, p.Kind} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	if len(p.Text) > 1<<20 || len(p.URL) > 2048 || len(p.Kind) > 32 {
		return false
	}
	for _, c := range []*model.Context{p.Parent, p.Quote} {
		if c == nil {
			continue
		}
		if !decimalID.MatchString(c.ID) || (c.Author != "" && !loginName.MatchString(c.Author)) || len(c.Text) > 1<<20 || len(c.URL) > 2048 {
			return false
		}
		for _, s := range []string{c.Text, c.URL} {
			if !utf8.ValidString(s) {
				return false
			}
		}
	}
	return true
}

func postURL(id, author string) string {
	if !loginName.MatchString(author) {
		return "https://x.com/i/web/status/" + id
	}
	return "https://x.com/" + author + "/status/" + id
}

func contextSnippet(text string) string {
	count, last := 0, 0
	for i := range text {
		if count == contextRunes {
			return text[:last] + "…"
		}
		last = i
		count++
	}
	return text
}
