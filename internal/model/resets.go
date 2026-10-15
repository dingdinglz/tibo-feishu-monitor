package model

import (
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const CodexResetsURL = "https://codex-resets.com"

// API IDs are opaque strings, not necessarily X snowflakes. In particular,
// observed IDs may contain punctuation such as an ISO timestamp's colons.
func validResetID(id string) bool {
	if !utf8.ValidString(id) || id == "" || utf8.RuneCountInString(id) > 64 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Codex data is already classified by the upstream service. It is not a
// TypeSafe assessment, nor a guarantee about an individual user's quota.
type CodexSource struct {
	Type   string `json:"type"`
	Author string `json:"author,omitempty"`
	URL    string `json:"url,omitempty"`
}

func (s CodexSource) Valid() bool {
	if s.Type != "x_post" && s.Type != "observed" {
		return false
	}
	if s.Type == "x_post" && (s.Author != "thsottiaux" || s.URL == "") {
		return false
	}
	if s.URL == "" {
		return true
	}
	u, err := url.Parse(s.URL)
	return err == nil && len(s.URL) <= 2048 && u.Scheme == "https" && u.User == nil &&
		(u.Host == "x.com" || u.Host == "twitter.com" || u.Host == "codex-resets.com")
}

func (s CodexSource) Link() string {
	if s.Valid() && s.URL != "" {
		return s.URL
	}
	return CodexResetsURL
}

type CodexReset struct {
	ID          string      `json:"id"`
	ResetType   string      `json:"reset_type"`
	AnnouncedAt time.Time   `json:"announced_at"`
	Text        string      `json:"text"`
	Source      CodexSource `json:"source"`
}

func (r CodexReset) Valid() bool {
	return validResetID(r.ID) && (r.ResetType == "regular" || r.ResetType == "banked") &&
		!r.AnnouncedAt.IsZero() && utf8.ValidString(r.Text) && len(r.Text) <= 1<<20 && r.Source.Valid()
}

type CodexScheduledReset struct {
	ID           string      `json:"id"`
	Status       string      `json:"status"`
	ResetType    string      `json:"reset_type"`
	AnnouncedAt  time.Time   `json:"announced_at"`
	ScheduledFor *time.Time  `json:"scheduled_for"`
	Text         string      `json:"text"`
	Source       CodexSource `json:"source"`
}

func (r CodexScheduledReset) Valid() bool {
	return r.Status == "scheduled" && (r.ScheduledFor == nil || !r.ScheduledFor.IsZero()) &&
		(CodexReset{ID: r.ID, ResetType: r.ResetType, AnnouncedAt: r.AnnouncedAt, Text: r.Text, Source: r.Source}).Valid()
}

type CodexWatch struct {
	Level              string      `json:"level"`
	ResetChancePercent *int        `json:"reset_chance_percent"`
	ForecastWindow     string      `json:"forecast_window"`
	ObservedAt         time.Time   `json:"observed_at"`
	ExpiresAt          time.Time   `json:"expires_at"`
	Text               string      `json:"text"`
	Source             CodexSource `json:"source"`
}

func (w CodexWatch) Valid() bool {
	return (w.Level == "elevated" || w.Level == "strong") &&
		(w.ResetChancePercent == nil || (*w.ResetChancePercent >= 0 && *w.ResetChancePercent <= 100)) &&
		!w.ObservedAt.IsZero() && w.ExpiresAt.After(w.ObservedAt) &&
		utf8.ValidString(w.Text) && len(w.Text) <= 1<<20 && utf8.ValidString(w.ForecastWindow) &&
		len(w.ForecastWindow) <= 1024 && w.Source.Valid()
}

type CodexStats struct {
	Total           int        `json:"total"`
	LastResetAt     *time.Time `json:"last_reset_at"`
	DaysSinceLast   *float64   `json:"days_since_last"`
	AvgIntervalDays *float64   `json:"avg_interval_days"`
}

func validDays(n *float64) bool {
	return n == nil || (!math.IsNaN(*n) && !math.IsInf(*n, 0) && *n >= 0)
}

func (s CodexStats) Valid() bool {
	return s.Total >= 0 && (s.LastResetAt == nil || !s.LastResetAt.IsZero()) &&
		validDays(s.DaysSinceLast) && validDays(s.AvgIntervalDays)
}

type CodexStatus struct {
	LatestReset    *CodexReset          `json:"latest_reset"`
	ScheduledReset *CodexScheduledReset `json:"scheduled_reset"`
	ActiveWatch    *CodexWatch          `json:"active_watch"`
	Stats          CodexStats           `json:"stats"`
	GeneratedAt    time.Time            `json:"generated_at"`
}

func (s CodexStatus) Valid() bool {
	return !s.GeneratedAt.IsZero() && s.Stats.Valid() &&
		(s.LatestReset == nil || s.LatestReset.Valid()) &&
		(s.ScheduledReset == nil || s.ScheduledReset.Valid()) &&
		(s.ActiveWatch == nil || s.ActiveWatch.Valid())
}

// A report is a durable snapshot, not a live countdown. Retries retain its
// original observation time so an old queued card cannot look newly fetched.
type CodexReport struct {
	Status              CodexStatus `json:"status"`
	LongestIntervalDays *float64    `json:"longest_interval_days"`
	AsOf                time.Time   `json:"as_of"`
	Date                string      `json:"date"`
	Timezone            string      `json:"timezone"`
}

func (r CodexReport) Valid() bool {
	loc, err := time.LoadLocation(r.Timezone)
	return err == nil && r.Timezone != "" && !r.AsOf.IsZero() &&
		r.Date == r.AsOf.In(loc).Format("2006-01-02") && r.Status.Valid() && validDays(r.LongestIntervalDays)
}
