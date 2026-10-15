package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tibo-monitor/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func fakeClient(status int, body string) *Client {
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testResponse(status, body), nil
	})
	return c
}

func destination() config.Destination {
	// Only fake round trippers use this deliberately non-routable test domain.
	return config.Destination{URL: "https://webhook.invalid/private-hook", Secret: "private-signing-secret"}
}

func sendError(t *testing.T, err error, code string, permanent bool) *SendError {
	t.Helper()
	var got *SendError
	if !errors.As(err, &got) {
		t.Fatalf("expected SendError, got %T", err)
	}
	if got.Code != code || got.Permanent != permanent {
		t.Fatalf("got code=%s permanent=%v, want %s %v", got.Code, got.Permanent, code, permanent)
	}
	if len(got.Error()) > 80 {
		t.Fatal("unbounded error string")
	}
	return got
}

func TestSignKnownVectorAndEveryAttempt(t *testing.T) {
	// Independent Python standard-library HMAC-SHA256 / Base64 vector, using
	// the timestamp and secret in Feishu's official examples. Message is empty.
	const want = "l1N0gAcBjdwBvGm1xMjOF0XSyaLRpR7tuO5dHfhAYc8="
	if got := sign("demo", "1599360473"); got != want {
		t.Fatalf("signature mismatch: %s", got)
	}
	c := New(time.Second)
	now := time.Unix(1599360473, 0)
	c.now = func() time.Time { return now }
	var attempts []textPayload
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected request protocol or token")
		}
		var payload textPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal("invalid request JSON")
		}
		attempts = append(attempts, payload)
		if len(attempts) == 1 {
			return testResponse(200, `{"code":11232}`), nil
		}
		return testResponse(200, `{"code":0}`), nil
	})
	d := destination()
	d.Secret = "demo"
	sendError(t, c.Send(context.Background(), d, "queued text"), "11232", false)
	now = now.Add(time.Second)
	d.Prefix = "new keyword"
	if err := c.Send(context.Background(), d, "queued text"); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Timestamp != "1599360473" || attempts[1].Timestamp != "1599360474" || attempts[0].Sign != want || attempts[0].Sign == attempts[1].Sign {
		t.Fatal("each attempt must use the current timestamp and a fresh signature")
	}
	if attempts[0].Content.Text != "Tibo 更新\nqueued text" || attempts[1].Content.Text != "new keyword\nqueued text" {
		t.Fatal("queued text must use the current destination prefix")
	}
}

func TestNoSecretAndMentionGuardOnWire(t *testing.T) {
	c := New(time.Second)
	d := destination()
	d.Secret = ""
	d.Prefix = "keyword <at user_id=\"all\">"
	text := "中文\U0001F642\\\"\x00\b\f\n\r\t<&>\u2028\u2029 <at user_id=\"person\">"
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.ContentLength != int64(len(wire)) || len(wire) > maxRequestBytes {
			t.Fatal("incorrect wire length")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal("invalid request JSON")
		}
		if _, exists := fields["sign"]; exists {
			t.Fatal("unsigned requests must omit sign")
		}
		if _, exists := fields["timestamp"]; exists {
			t.Fatal("unsigned requests must omit timestamp")
		}
		var payload textPayload
		if err := json.Unmarshal(wire, &payload); err != nil {
			t.Fatal(err)
		}
		// U+200B after '<' deliberately prevents literal <at from mentioning
		// everyone or an individual; all other original characters survive.
		want := strings.ReplaceAll(d.Prefix+"\n"+text, "<at", "<\u200bat")
		if payload.Content.Text != want || strings.Contains(payload.Content.Text, "<at") {
			t.Fatal("mention guard or Unicode/JSON round trip failed")
		}
		if !bytes.Contains(wire, []byte(`\u0000`)) || !bytes.Contains(wire, []byte(`\u003c`)) {
			t.Fatal("JSON control/HTML escaping was not exercised")
		}
		return testResponse(200, `{"StatusCode":0}`), nil
	})
	if err := c.Send(context.Background(), d, text); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadExactByteLimit(t *testing.T) {
	for _, signed := range []bool{false, true} {
		t.Run(strconv.FormatBool(signed), func(t *testing.T) {
			c := New(time.Second)
			c.now = func() time.Time { return time.Unix(1599360473, 0) }
			d := destination()
			d.Prefix, d.Secret = "x", ""
			base := textPayload{MsgType: "text"}
			base.Content.Text = "x\n"
			if signed {
				d.Secret = "demo"
				base.Timestamp = "1599360473"
				base.Sign = "l1N0gAcBjdwBvGm1xMjOF0XSyaLRpR7tuO5dHfhAYc8="
			}
			encoded, _ := json.Marshal(base)
			n := maxRequestBytes - len(encoded)
			calls := 0
			c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				wire, _ := io.ReadAll(r.Body)
				if len(wire) != maxRequestBytes {
					t.Fatalf("got wire bytes %d", len(wire))
				}
				return testResponse(200, `{"code":0}`), nil
			})
			if err := c.Send(context.Background(), d, strings.Repeat("a", n)); err != nil {
				t.Fatal(err)
			}
			sendError(t, c.Send(context.Background(), d, strings.Repeat("a", n+1)), "payload_too_large", true)
			// These have small UTF-8 byte counts but expand significantly in JSON.
			for _, text := range []string{strings.Repeat("\x00", 3400), strings.Repeat("\\", 10000), strings.Repeat("<at", 1900)} {
				sendError(t, c.Send(context.Background(), d, text), "payload_too_large", true)
			}
			if calls != 1 {
				t.Fatal("oversized payload reached transport")
			}
		})
	}
}

func TestResponseStylesAndStrictCodes(t *testing.T) {
	tests := []struct {
		name, body, code string
		permanent        bool
	}{
		{"modern", `{"code":0,"msg":"success","data":{},"StatusCode":0,"StatusMessage":"success"}`, "", false},
		{"legacy", `{"StatusCode":0,"StatusMessage":"success"}`, "", false},
		{"ignore_legacy", `{"code":0,"StatusCode":999}`, "", false},
		{"ignore_null_legacy", `{"code":0,"StatusCode":null}`, "", false},
		{"missing", `{}`, "invalid_response", false},
		{"null_body", `null`, "invalid_response", false},
		{"array", `[{"code":0}]`, "invalid_response", false},
		{"empty", ``, "invalid_response", false},
		{"wrong_case", `{"Code":0}`, "invalid_response", false},
		{"null", `{"code":null,"StatusCode":0}`, "invalid_response", false},
		{"legacy_null", `{"StatusCode":null}`, "invalid_response", false},
		{"string", `{"code":"0","StatusCode":0}`, "invalid_response", false},
		{"bool", `{"code":false,"StatusCode":0}`, "invalid_response", false},
		{"fraction", `{"code":0.0}`, "invalid_response", false},
		{"exponent", `{"code":0e0}`, "invalid_response", false},
		{"overflow", `{"code":9999999999999999999999999}`, "invalid_response", false},
		{"garbage", `{"code":0} {"code":0}`, "invalid_response", false},
		{"invalid_utf8", "{\"code\":0,\"msg\":\"\xff\"}", "invalid_response", false},
		{"precedence", `{"code":19021,"StatusCode":0}`, "19021", true},
		{"bad_request", `{"code":9499,"msg":"private upstream text"}`, "9499", true},
		{"signature", `{"code":19021}`, "19021", true},
		{"ip", `{"code":19022}`, "19022", true},
		{"keyword", `{"code":19024}`, "19024", true},
		{"legacy_keyword", `{"StatusCode":19024}`, "19024", true},
		{"service_limit", `{"code":11232}`, "11232", false},
		{"gateway_limit", `{"code":99991400}`, "99991400", false},
		{"unknown", `{"code":87654321,"msg":"private upstream text"}`, "business_error", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fakeClient(200, tt.body).Send(context.Background(), destination(), "text")
			if tt.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			got := sendError(t, err, tt.code, tt.permanent)
			if strings.Contains(got.Error(), "private") || strings.Contains(got.Error(), "87654321") {
				t.Fatal("upstream response leaked into error")
			}
		})
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	for _, status := range []int{201, 202, 299} {
		if err := fakeClient(status, `{"code":0}`).Send(context.Background(), destination(), "text"); err != nil {
			t.Fatal(err)
		}
	}
	sendError(t, fakeClient(204, "").Send(context.Background(), destination(), "text"), "invalid_response", false)
	for _, status := range []int{300, 301, 307, 400, 401, 403, 404, 408, 413, 429, 500, 502, 503, 504, 599} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			permanent := status != 408 && status != 429 && status < 500
			// An explicit business success cannot override a failed HTTP status.
			err := fakeClient(status, `{"code":0}`).Send(context.Background(), destination(), "text")
			sendError(t, err, "http_"+strconv.Itoa(status), permanent)
		})
	}
	for _, code := range []int{11232, 99991400} {
		body := fmt.Sprintf(`{"code":%d}`, code)
		sendError(t, fakeClient(400, body).Send(context.Background(), destination(), "text"), strconv.Itoa(code), false)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tt := range []struct {
		name, retry, reset string
		want               time.Duration
	}{
		{"seconds", "12", "5", 12 * time.Second},
		{"reset_longer", "3", "30", 30 * time.Second},
		{"date", now.Add(90 * time.Second).Format(http.TimeFormat), "30", 90 * time.Second},
		{"date_shorter", now.Add(10 * time.Second).Format(http.TimeFormat), "30", 30 * time.Second},
		{"past", now.Add(-time.Minute).Format(http.TimeFormat), "", 0},
		{"invalid", "not a date", "-5", 0},
		{"fraction", "1.5", "+5", 0},
		{"space", " 12 ", " 15 ", 15 * time.Second},
		{"overflow", "999999999999999999999999999999999", "0", time.Duration(1<<63 - 1)},
		{"invalid_overflow", "999999999999999999999999bad", "2", 2 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := New(time.Second)
			c.now = func() time.Time { return now }
			c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				r := testResponse(429, `{"msg":"not required"}`)
				r.Header.Set("Retry-After", tt.retry)
				r.Header.Set("x-ogw-ratelimit-reset", tt.reset)
				return r, nil
			})
			got := sendError(t, c.Send(context.Background(), destination(), "text"), "http_429", false)
			if got.RetryAfter != tt.want {
				t.Fatalf("wait %v, want %v", got.RetryAfter, tt.want)
			}
		})
	}
	h := make(http.Header)
	h.Add("Retry-After", "2")
	h.Add("Retry-After", "15")
	h.Add("x-ogw-ratelimit-reset", "8")
	h.Add("x-ogw-ratelimit-reset", "20")
	if got := retryDelay(h, now); got != 20*time.Second {
		t.Fatalf("multiple-header wait %v", got)
	}
}

func TestDirectTransportAndRedirectBlocked(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "http://proxy-user:proxy-secret@proxy.invalid:8123")
	}
	c := New(321 * time.Millisecond)
	defer c.httpClient.CloseIdleConnections()
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || c.httpClient.Timeout != 321*time.Millisecond {
		t.Fatal("client must use its own direct transport and exact configured timeout")
	}
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL+"/private-redirect-target")
				w.WriteHeader(status)
			}))
			defer source.Close()
			d := destination()
			d.URL = source.URL + "/private-hook"
			got := sendError(t, c.Send(context.Background(), d, "private-body"), "http_"+strconv.Itoa(status), true)
			if strings.Contains(got.Error(), source.URL) || strings.Contains(got.Error(), "private") || followed.Load() != 0 {
				t.Fatal("redirect followed or secret leaked")
			}
		})
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	for _, flushBody := range []bool{false, true} {
		t.Run(strconv.FormatBool(flushBody), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if flushBody {
					_, _ = io.WriteString(w, `{"code":`)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			c := New(50 * time.Millisecond)
			defer c.httpClient.CloseIdleConnections()
			d := destination()
			d.URL = srv.URL
			code := "timeout"
			if flushBody {
				code = "response_read"
			}
			start := time.Now()
			sendError(t, c.Send(context.Background(), d, "text"), code, false)
			if time.Since(start) > 2*time.Second {
				t.Fatal("configured request/body timeout was not enforced")
			}
		})
	}
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sendError(t, c.Send(ctx, destination(), "text"), "canceled", false)
}

type trackedBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *trackedBody) Close() error { b.closed = true; return nil }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("private body/proxy/webhook details")
}

func TestResponseReadBoundAndClose(t *testing.T) {
	for _, status := range []int{200, 429} {
		body := &trackedBody{reader: strings.NewReader(strings.Repeat("x", 2*maxResponseBytes))}
		c := New(time.Second)
		c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			r := testResponse(status, "")
			r.Body = body
			r.Header.Set("Retry-After", "19")
			return r, nil
		})
		code := "response_too_large"
		if status == 429 {
			code = "http_429"
		}
		got := sendError(t, c.Send(context.Background(), destination(), "text"), code, false)
		if !body.closed || body.read != maxResponseBytes+1 || got.RetryAfter != 19*time.Second {
			t.Fatal("response read must be bounded, closed, and preserve retry headers")
		}
	}
	body := &trackedBody{reader: failingReader{}}
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})
	sendError(t, c.Send(context.Background(), destination(), "text"), "response_read", false)
	if !body.closed {
		t.Fatal("failed response body not closed")
	}
}

func TestErrorsCannotExposeSecrets(t *testing.T) {
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxy http://user:proxy-secret@proxy.invalid:8888 webhook %s secret private-signing-secret", r.URL)
	})
	got := sendError(t, c.Send(context.Background(), destination(), "text"), "network", false)
	for _, secret := range []string{"http", "proxy", "private", "webhook.invalid", "signing"} {
		if strings.Contains(got.Error(), secret) {
			t.Fatal("sensitive network error exposed")
		}
	}
	d := destination()
	d.URL = "https://private.invalid/\nprivate-hook"
	sendError(t, c.Send(context.Background(), d, "text"), "invalid_request", true)
	for _, code := range []string{"https://private.invalid/hook", "http_200\nprivate", "http_999", "private-signing-secret"} {
		if got := (&SendError{Code: code}).Error(); got != "feishu: send_error" {
			t.Fatal("caller-supplied error code was not sanitized")
		}
	}
	var nilError *SendError
	if nilError.Error() != "feishu: send_error" {
		t.Fatal("nil SendError should be safe")
	}
}

func TestInvalidInputsDoNotReachTransport(t *testing.T) {
	c := New(time.Second)
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid input reached transport")
		return nil, errors.New("unreachable")
	})
	d := destination()
	sendError(t, c.Send(context.Background(), d, "\xff"), "invalid_utf8", true)
	d.Prefix = "\xff"
	sendError(t, c.Send(context.Background(), d, "text"), "invalid_utf8", true)
	d.Prefix = strings.Repeat("x", maxPrefixBytes+1)
	sendError(t, c.Send(context.Background(), d, "text"), "prefix_too_large", true)
	d.Prefix = ""
	for _, u := range []string{"file:///private-hook", "/relative", "https://user:private@webhook.invalid/hook", "https://webhook.invalid/hook#private"} {
		d.URL = u
		sendError(t, c.Send(context.Background(), d, "text"), "invalid_request", true)
	}
}
