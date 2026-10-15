// Package feishu sends individual post cards and operational text alerts to custom group-bot webhooks.
package feishu

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tibo-monitor/internal/config"
)

const (
	maxRequestBytes  = 20000 // Conservative interpretation of the documented 20 KB.
	maxResponseBytes = 16 * 1024
	maxPrefixBytes   = 512
	defaultPrefix    = "Tibo 更新"
)

// SendError deliberately retains neither the underlying error nor upstream text.
// Permanent means retrying without fixing the request/configuration is not useful.
// RetryAfter is a minimum wait; callers implement scheduling and bounded retries.
type SendError struct {
	Code       string
	Permanent  bool
	RetryAfter time.Duration
}

func (e *SendError) Error() string {
	if e == nil {
		return "feishu: send_error"
	}
	// Even a caller-created SendError cannot put credentials into Error().
	switch e.Code {
	case "network", "timeout", "canceled", "invalid_request", "invalid_utf8",
		"prefix_too_large", "payload_too_large", "payload_encoding",
		"response_read", "response_too_large", "invalid_response", "business_error",
		"11232", "99991400", "9499", "19021", "19022", "19024":
		return "feishu: " + e.Code
	}
	if len(e.Code) == len("http_200") && strings.HasPrefix(e.Code, "http_") {
		if n, err := strconv.Atoi(e.Code[5:]); err == nil && n >= 100 && n <= 599 {
			return "feishu: http_" + strconv.Itoa(n)
		}
	}
	return "feishu: send_error"
}

// Client is safe for concurrent use. It does not retry internally: a lost response
// can mean the message was delivered, and custom bots provide no idempotency key.
type Client struct {
	httpClient *http.Client
	now        func() time.Time
}

func New(timeout time.Duration) *Client {
	// Do not clone a possibly customized global transport, and do not inherit
	// HTTP_PROXY/HTTPS_PROXY/ALL_PROXY from the scraper's environment.
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
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

type textPayload struct {
	Timestamp string `json:"timestamp,omitempty"`
	Sign      string `json:"sign,omitempty"`
	MsgType   string `json:"msg_type"`
	Content   struct {
		Text string `json:"text"`
	} `json:"content"`
}

// Send adds the current destination's keyword prefix at delivery time, never when
// queueing. Each attempt signs its own current Unix-second timestamp.
func (c *Client) Send(ctx context.Context, d config.Destination, text string) error {
	prefix := d.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	if !utf8.ValidString(prefix) || !utf8.ValidString(text) {
		return &SendError{Code: "invalid_utf8", Permanent: true}
	}
	if len(prefix) > maxPrefixBytes {
		return &SendError{Code: "prefix_too_large", Permanent: true}
	}
	record, isCard, err := decodeCard(text)
	if err != nil {
		return err
	}
	var body []byte
	if isCard {
		body, err = c.cardBody(prefix, d.Secret, record)
		if err != nil {
			return err
		}
	} else {
		if len(text) > maxRequestBytes {
			return &SendError{Code: "payload_too_large", Permanent: true}
		}
		payload := textPayload{MsgType: "text"}
		payload.Content.Text = neutralizeMentions(prefix + "\n" + text)
		if d.Secret != "" {
			payload.Timestamp = strconv.FormatInt(c.now().Unix(), 10)
			payload.Sign = sign(d.Secret, payload.Timestamp)
		}
		body, err = json.Marshal(payload)
		if err != nil {
			return &SendError{Code: "payload_encoding", Permanent: true}
		}
	}
	if len(body) > maxRequestBytes {
		return &SendError{Code: "payload_too_large", Permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return &SendError{Code: "invalid_request", Permanent: true}
	}
	// config.Load validates official webhook hosts. Accept HTTP here as well so
	// isolated local test servers can exercise the actual HTTP stack.
	if (req.URL.Scheme != "https" && req.URL.Scheme != "http") || req.URL.Host == "" || req.URL.User != nil || req.URL.Fragment != "" {
		return &SendError{Code: "invalid_request", Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		code := "network"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "timeout"
		} else if errors.Is(err, context.Canceled) {
			code = "canceled"
		}
		return &SendError{Code: code}
	}
	defer resp.Body.Close()
	wait := retryDelay(resp.Header, c.now())
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	validSize := len(data) <= maxResponseBytes
	code, validCode := responseCode(data)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result := &SendError{
			Code:       "http_" + strconv.Itoa(resp.StatusCode),
			Permanent:  resp.StatusCode != 408 && resp.StatusCode != 429 && !(resp.StatusCode >= 500 && resp.StatusCode <= 599),
			RetryAfter: wait,
		}
		// Older gateways can return HTTP 400 together with a rate-limit code.
		if readErr == nil && validSize && validCode && (code == 11232 || code == 99991400) {
			result.Code = strconv.FormatInt(code, 10)
			result.Permanent = false
		}
		return result
	}
	if readErr != nil {
		return &SendError{Code: "response_read", RetryAfter: wait}
	}
	if !validSize {
		return &SendError{Code: "response_too_large", RetryAfter: wait}
	}
	if !validCode {
		return &SendError{Code: "invalid_response", RetryAfter: wait}
	}
	if code == 0 {
		return nil
	}
	switch code {
	case 11232, 99991400:
		return &SendError{Code: strconv.FormatInt(code, 10), RetryAfter: wait}
	case 9499, 19021, 19022, 19024:
		return &SendError{Code: strconv.FormatInt(code, 10), Permanent: true, RetryAfter: wait}
	default:
		// Unknown business failures need inspection, not unlimited blind retries.
		// Do not reflect even an unknown upstream code into the error string.
		return &SendError{Code: "business_error", Permanent: true, RetryAfter: wait}
	}
}

func sign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	// Feishu signs the empty message, using timestamp + newline + secret as KEY.
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func neutralizeMentions(text string) string {
	return strings.ReplaceAll(text, "<at", "<\u200bat")
}

func responseCode(data []byte) (int64, bool) {
	if !utf8.Valid(data) {
		return 0, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return 0, false
	}
	raw, exists := fields["code"]
	if !exists {
		raw, exists = fields["StatusCode"]
	}
	if !exists {
		return 0, false
	}
	// A pointer distinguishes explicit zero from null. int64 also rejects
	// strings, booleans, fractions, exponent notation and integer overflow.
	var code *int64
	if err := json.Unmarshal(raw, &code); err != nil || code == nil {
		return 0, false
	}
	return *code, true
}

func retryDelay(headers http.Header, now time.Time) time.Duration {
	var longest time.Duration
	for _, value := range headers.Values("Retry-After") {
		value = strings.TrimSpace(value)
		wait, ok := secondsDelay(value)
		if !ok {
			if date, err := http.ParseTime(value); err == nil {
				wait = date.Sub(now)
			}
		}
		if wait > longest {
			longest = wait
		}
	}
	for _, value := range headers.Values("X-Ogw-Ratelimit-Reset") {
		if wait, ok := secondsDelay(strings.TrimSpace(value)); ok && wait > longest {
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
