package model

import (
	"math"
	"time"
)

// All X IDs cross process and storage boundaries as decimal strings.
type Context struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Text      string `json:"text"`
	URL       string `json:"url"`
	Available bool   `json:"available"`
}

type Post struct {
	ID        string    `json:"id"`
	AuthorID  string    `json:"author_id"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	Text      string    `json:"text"`
	URL       string    `json:"url"`
	Kind      string    `json:"kind"` // post, reply, quote
	ReplyToID string    `json:"reply_to_id,omitempty"`
	Parent    *Context  `json:"parent,omitempty"`
	Quote     *Context  `json:"quote,omitempty"`
}

// ResetAssessment is an AI estimate, not confirmation that a reset happened.
// A nil probability means unavailable, which must never be displayed as 0%.
type ResetAssessment struct {
	Probability  *float64  `json:"probability,omitempty"`
	Model        string    `json:"model,omitempty"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
	HistoryCount int       `json:"history_count"`
}

func (a ResetAssessment) Valid() bool {
	return a.Probability != nil && !math.IsNaN(*a.Probability) && !math.IsInf(*a.Probability, 0) &&
		*a.Probability >= 0 && *a.Probability <= 1 && a.Model != "" && len(a.Model) <= 128 &&
		!a.EvaluatedAt.IsZero() && a.HistoryCount >= 0 && a.HistoryCount <= 10
}

type FetchRequest struct {
	AccountsDB     string             `json:"accounts_db"`
	UserID         string             `json:"user_id"`
	Username       string             `json:"username"`
	Proxy          string             `json:"proxy"`
	Since          time.Time          `json:"since"`
	Cursor         string             `json:"cursor"`
	MaxPages       int                `json:"max_pages"`
	TimeoutSeconds float64            `json:"timeout_seconds"`
	WaitSeconds    float64            `json:"wait_seconds"`
	ContextLimit   int                `json:"context_limit"`
	CachedContexts map[string]Context `json:"cached_contexts"`
	IncludeLatest  bool               `json:"include_latest,omitempty"` // debug fallback, independent of since
}

type FetchResult struct {
	Version           int       `json:"version"`
	Posts             []Post    `json:"posts"`
	Latest            *Post     `json:"latest,omitempty"` // target's latest observed post, even outside the window
	Contexts          []Context `json:"contexts"`
	HeadOK            bool      `json:"head_ok"`
	Complete          bool      `json:"complete"`
	Cursor            string    `json:"cursor"`
	Pages             int       `json:"pages"`
	Warnings          []string  `json:"warnings"`
	Error             string    `json:"error"` // fixed, sanitized error code, never raw upstream output
	RetryAfterSeconds int       `json:"retry_after_seconds"`
}
