// Package codexresets provides read-only access to the Codex Resets public API.
package codexresets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"tibo-monitor/internal/model"
)

const (
	productionBaseURL = "https://codex-resets.com"
	defaultTimeout    = 30 * time.Second
	maxResponseBytes  = 2 * 1024 * 1024
	pageSize          = 100
	maxPages          = 100
	maxCursorBytes    = 1024
)

// Error retains no upstream body, URL, or underlying transport error. RetryAfter
// is a minimum wait supplied by the server; the caller owns retries/backoff.
type Error struct {
	Code       string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil {
		return "codexresets: request_error"
	}
	switch e.Code {
	case "network", "timeout", "canceled", "invalid_request", "response_read",
		"response_too_large", "invalid_response", "invalid_cursor", "cursor_repeated", "page_limit":
		return "codexresets: " + e.Code
	}
	if len(e.Code) == len("http_200") && strings.HasPrefix(e.Code, "http_") {
		if code, err := strconv.Atoi(e.Code[5:]); err == nil && code >= 100 && code <= 599 {
			return "codexresets: http_" + strconv.Itoa(code)
		}
	}
	return "codexresets: request_error"
}

// Is preserves context error checks without retaining an unsafe wrapped error.
func (e *Error) Is(target error) bool {
	return e != nil && ((e.Code == "canceled" && target == context.Canceled) ||
		(e.Code == "timeout" && target == context.DeadlineExceeded))
}

// Client is safe for concurrent use. It does not retry or cache responses.
// Polling and daily reports share server cooldowns without serializing HTTP.
type Client struct {
	baseURL       string
	httpClient    *http.Client
	timeout       time.Duration
	now           func() time.Time
	cooldownMu    sync.Mutex
	cooldownUntil time.Time
	cooldownCode  string
}

// New applies timeout to the entire method call, including all pages. A
// nonpositive timeout uses a bounded default rather than disabling the budget.
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// Do not inherit customized global transports or environment proxy settings.
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		baseURL: productionBaseURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout: timeout,
		now:     time.Now,
	}
}

func (c *Client) Status(ctx context.Context) (model.CodexStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := c.get(ctx, "/api/v1/status", nil)
	if err != nil {
		return model.CodexStatus{}, err
	}
	status, valid := decodeStatus(body)
	if err := ctx.Err(); err != nil {
		return model.CodexStatus{}, requestError(ctx, err, "network")
	}
	if !valid {
		return model.CodexStatus{}, &Error{Code: "invalid_response"}
	}
	return status, nil
}

// Resets returns all matching history, in the API's ascending order. A zero
// from omits the filter. Errors never return a partial history as a success.
func (c *Client) Resets(ctx context.Context, from time.Time) ([]model.CodexReset, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	query := url.Values{"limit": {strconv.Itoa(pageSize)}, "order": {"asc"}}
	if !from.IsZero() {
		query.Set("from", from.Format(time.RFC3339Nano))
	}
	resets := make([]model.CodexReset, 0)
	seen := make(map[string]struct{})
	for page := 0; page < maxPages; page++ {
		body, err := c.get(ctx, "/api/v1/resets", query)
		if err != nil {
			return nil, err
		}
		result, valid := decodePage(body)
		if err := ctx.Err(); err != nil {
			return nil, requestError(ctx, err, "network")
		}
		if !valid {
			return nil, &Error{Code: "invalid_response"}
		}
		resets = append(resets, result.resets...)
		if !result.hasMore {
			return resets, nil
		}
		if result.nextCursor == nil || strings.TrimSpace(*result.nextCursor) == "" || len(*result.nextCursor) > maxCursorBytes {
			return nil, &Error{Code: "invalid_cursor"}
		}
		cursor := *result.nextCursor
		if _, exists := seen[cursor]; exists {
			return nil, &Error{Code: "cursor_repeated"}
		}
		seen[cursor] = struct{}{}
		query.Set("cursor", cursor)
	}
	return nil, &Error{Code: "page_limit"}
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, requestError(ctx, err, "network")
	}
	if err := c.cooldown(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, &Error{Code: "invalid_request"}
	}
	// HTTP is accepted for isolated local test servers; production is fixed HTTPS.
	if (req.URL.Scheme != "https" && req.URL.Scheme != "http") || req.URL.Host == "" || req.URL.User != nil || req.URL.Fragment != "" || req.URL.RawQuery != "" {
		return nil, &Error{Code: "invalid_request"}
	}
	req.URL.RawQuery = query.Encode()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tibo-monitor")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, requestError(ctx, err, "network")
	}
	defer resp.Body.Close()
	wait := retryDelay(resp.Header, c.now())
	if resp.StatusCode != http.StatusOK {
		// Problem details and redirect destinations are intentionally not read.
		err := &Error{Code: "http_" + strconv.Itoa(resp.StatusCode), RetryAfter: wait}
		c.rememberCooldown(err)
		return nil, err
	}
	if resp.ContentLength > maxResponseBytes {
		return nil, &Error{Code: "response_too_large"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, requestError(ctx, err, "response_read")
	}
	if len(body) > maxResponseBytes {
		return nil, &Error{Code: "response_too_large"}
	}
	if !utf8.Valid(body) {
		return nil, &Error{Code: "invalid_response"}
	}
	return body, nil
}

func (c *Client) cooldown() error {
	c.cooldownMu.Lock()
	defer c.cooldownMu.Unlock()
	if remaining := c.cooldownUntil.Sub(c.now()); remaining > 0 {
		return &Error{Code: c.cooldownCode, RetryAfter: remaining}
	}
	return nil
}

func (c *Client) rememberCooldown(err *Error) {
	if err.RetryAfter <= 0 {
		return
	}
	c.cooldownMu.Lock()
	defer c.cooldownMu.Unlock()
	until := c.now().Add(err.RetryAfter)
	if until.After(c.cooldownUntil) {
		c.cooldownUntil, c.cooldownCode = until, err.Code
	}
}

func requestError(ctx context.Context, err error, fallback string) *Error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	var timeout net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Code: "canceled"}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return &Error{Code: "timeout"}
	default:
		return &Error{Code: fallback}
	}
}

// requiredObject checks required non-null fields. Required nullable fields use
// hasFields separately, so omission cannot masquerade as an explicit null.
// Unknown fields are tolerated for forward-compatible API additions.
func requiredObject(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || !hasFields(fields, names...) {
		return nil, false
	}
	for _, name := range names {
		if isNull(fields[name]) {
			return nil, false
		}
	}
	return fields, true
}

func hasFields(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, exists := fields[name]; !exists {
			return false
		}
	}
	return true
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeMeta(raw json.RawMessage) (time.Time, bool) {
	var meta struct {
		APIVersion  string    `json:"api_version"`
		GeneratedAt time.Time `json:"generated_at"`
	}
	_, valid := requiredObject(raw, "api_version", "generated_at")
	if !valid || json.Unmarshal(raw, &meta) != nil || meta.APIVersion != "v1" || meta.GeneratedAt.IsZero() {
		return time.Time{}, false
	}
	return meta.GeneratedAt, true
}

func validSourceShape(raw json.RawMessage) bool {
	fields, valid := requiredObject(raw, "type")
	if !valid {
		return false
	}
	var kind string
	if json.Unmarshal(fields["type"], &kind) != nil {
		return false
	}
	if kind == "x_post" {
		if _, valid := requiredObject(raw, "author", "url"); !valid {
			return false
		}
	}
	// Observed sources may omit author and URL, but present values must be strings.
	for _, name := range []string{"author", "url"} {
		if value, exists := fields[name]; exists {
			var text string
			if isNull(value) || json.Unmarshal(value, &text) != nil {
				return false
			}
		}
	}
	return true
}

func decodeReset(raw json.RawMessage) (model.CodexReset, bool) {
	var reset model.CodexReset
	fields, valid := requiredObject(raw, "id", "reset_type", "announced_at", "text", "source")
	if !valid || !validSourceShape(fields["source"]) || json.Unmarshal(raw, &reset) != nil {
		return reset, false
	}
	return reset, !reset.AnnouncedAt.IsZero() && reset.Valid()
}

func decodeScheduled(raw json.RawMessage) (model.CodexScheduledReset, bool) {
	var reset model.CodexScheduledReset
	fields, valid := requiredObject(raw, "id", "status", "reset_type", "announced_at", "text", "source")
	if !valid || !hasFields(fields, "scheduled_for") || !validSourceShape(fields["source"]) || json.Unmarshal(raw, &reset) != nil {
		return reset, false
	}
	return reset, !reset.AnnouncedAt.IsZero() && (reset.ScheduledFor == nil || !reset.ScheduledFor.IsZero()) && reset.Valid()
}

func decodeWatch(raw json.RawMessage) (model.CodexWatch, bool) {
	var watch model.CodexWatch
	fields, valid := requiredObject(raw, "level", "forecast_window", "observed_at", "expires_at", "text", "source")
	if !valid || !hasFields(fields, "reset_chance_percent") || !validSourceShape(fields["source"]) || json.Unmarshal(raw, &watch) != nil {
		return watch, false
	}
	return watch, !watch.ObservedAt.IsZero() && !watch.ExpiresAt.IsZero() && watch.Valid()
}

func decodeStats(raw json.RawMessage) (model.CodexStats, bool) {
	var stats model.CodexStats
	fields, valid := requiredObject(raw, "total")
	if !valid || !hasFields(fields, "last_reset_at", "days_since_last", "avg_interval_days") || json.Unmarshal(raw, &stats) != nil {
		return stats, false
	}
	return stats, (stats.LastResetAt == nil || !stats.LastResetAt.IsZero()) && stats.Valid()
}

func decodeStatus(body []byte) (model.CodexStatus, bool) {
	var status model.CodexStatus
	envelope, valid := requiredObject(body, "data", "meta")
	if !valid {
		return status, false
	}
	if status.GeneratedAt, valid = decodeMeta(envelope["meta"]); !valid {
		return status, false
	}
	data, valid := requiredObject(envelope["data"], "stats")
	if !valid || !hasFields(data, "latest_reset", "scheduled_reset", "active_watch") {
		return status, false
	}
	if !isNull(data["latest_reset"]) {
		reset, valid := decodeReset(data["latest_reset"])
		if !valid {
			return status, false
		}
		status.LatestReset = &reset
	}
	if !isNull(data["scheduled_reset"]) {
		reset, valid := decodeScheduled(data["scheduled_reset"])
		if !valid {
			return status, false
		}
		status.ScheduledReset = &reset
	}
	if !isNull(data["active_watch"]) {
		watch, valid := decodeWatch(data["active_watch"])
		if !valid {
			return status, false
		}
		status.ActiveWatch = &watch
	}
	if status.Stats, valid = decodeStats(data["stats"]); !valid {
		return status, false
	}
	return status, status.Valid()
}

type resetPage struct {
	resets     []model.CodexReset
	hasMore    bool
	nextCursor *string
}

func decodePage(body []byte) (resetPage, bool) {
	var page resetPage
	envelope, valid := requiredObject(body, "data", "pagination", "meta")
	if !valid {
		return page, false
	}
	if _, valid := decodeMeta(envelope["meta"]); !valid {
		return page, false
	}
	fields, valid := requiredObject(envelope["pagination"], "has_more")
	if !valid || !hasFields(fields, "next_cursor") {
		return page, false
	}
	if json.Unmarshal(fields["has_more"], &page.hasMore) != nil || json.Unmarshal(fields["next_cursor"], &page.nextCursor) != nil {
		return page, false
	}
	var rows []json.RawMessage
	if json.Unmarshal(envelope["data"], &rows) != nil || len(rows) > pageSize {
		return page, false
	}
	page.resets = make([]model.CodexReset, 0, len(rows))
	for _, row := range rows {
		reset, valid := decodeReset(row)
		if !valid {
			return page, false
		}
		page.resets = append(page.resets, reset)
	}
	return page, true
}

func retryDelay(headers http.Header, now time.Time) time.Duration {
	var longest time.Duration
	for _, value := range headers.Values("Retry-After") {
		value = strings.TrimSpace(value)
		wait, valid := secondsDelay(value)
		if !valid {
			if date, err := http.ParseTime(value); err == nil {
				wait = date.Sub(now)
			}
		}
		if wait > longest {
			longest = wait
		}
	}
	return longest
}

func secondsDelay(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	const maxDuration = time.Duration(1<<63 - 1)
	const maxSeconds = uint64(maxDuration / time.Second)
	var seconds uint64
	overflow := false
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		digit := uint64(ch - '0')
		if seconds > (maxSeconds-digit)/10 {
			overflow = true
		}
		if !overflow {
			seconds = seconds*10 + digit
		}
	}
	if overflow {
		return maxDuration, true
	}
	return time.Duration(seconds) * time.Second, true
}
