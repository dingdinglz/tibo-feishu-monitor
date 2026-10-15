package codexresets

import (
	"context"
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
)

// These fixtures preserve the public API's actual field names and nulls. They
// are embedded so the tests never depend on live requests or /tmp samples.
const (
	sampleMeta     = `{"api_version":"v1","generated_at":"2026-10-15T09:37:51.309Z"}`
	sampleSource   = `{"type":"x_post","author":"thsottiaux","url":"https://x.com/thsottiaux/status/2110618732666257557"}`
	sampleReset    = `{"id":"2110618732666257557","reset_type":"regular","announced_at":"2026-10-15T06:28:13.000Z","text":"Done and dusted https://t.co/0fehr43VS4","source":` + sampleSource + `}`
	observedReset  = `{"id":"observed-20260929T190000Z","reset_type":"banked","announced_at":"2026-09-29T19:00:00.000Z","text":"@theo Shhhhhh","source":{"type":"observed","url":"https://x.com/thsottiaux/status/2105120226027450685"}}`
	sampleStats    = `{"total":62,"last_reset_at":"2026-10-15T06:28:13.000Z","days_since_last":0.1,"avg_interval_days":6.4}`
	emptyStats     = `{"total":0,"last_reset_at":null,"days_since_last":null,"avg_interval_days":null}`
	sampleStatus   = `{"data":{"latest_reset":` + sampleReset + `,"scheduled_reset":null,"active_watch":null,"stats":` + sampleStats + `},"meta":` + sampleMeta + `}`
	sampleSchedule = `{"id":"2110618732666257557","status":"scheduled","reset_type":"banked","announced_at":"2026-10-15T06:28:13Z","scheduled_for":"2026-10-16T00:00:00Z","text":"A banked reset is scheduled.","source":` + sampleSource + `}`
	sampleWatch    = `{"level":"elevated","reset_chance_percent":65,"forecast_window":"next 24 hours","observed_at":"2026-10-15T06:28:13Z","expires_at":"2026-10-16T06:28:13Z","text":"A reset may happen.","source":` + sampleSource + `}`
)

func statusJSON(latest, scheduled, watch, stats string) string {
	return `{"data":{"latest_reset":` + latest + `,"scheduled_reset":` + scheduled + `,"active_watch":` + watch + `,"stats":` + stats + `},"meta":` + sampleMeta + `}`
}

func pageJSON(data string, more bool, cursorJSON string) string {
	return `{"data":` + data + `,"pagination":{"has_more":` + strconv.FormatBool(more) + `,"next_cursor":` + cursorJSON + `},"meta":` + sampleMeta + `}`
}

func testClient(t *testing.T, timeout time.Duration, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := New(timeout)
	client.baseURL = server.URL
	t.Cleanup(client.httpClient.CloseIdleConnections)
	return client
}

func checkRequest(t *testing.T, r *http.Request, path string) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != path {
		t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, path)
	}
	if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "tibo-monitor" {
		t.Error("missing API request headers")
	}
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		t.Error("read-only API request has a body")
	}
}

func clientError(t *testing.T, err error, code string) *Error {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) {
		t.Fatalf("want *Error with code %s, got %T (%v)", code, err, err)
	}
	if got.Code != code || got.Error() != "codexresets: "+code {
		t.Fatalf("got error %q (%s), want %s", got.Error(), got.Code, code)
	}
	return got
}

func invoke(c *Client, ctx context.Context, method string) error {
	if method == "status" {
		_, err := c.Status(ctx)
		return err
	}
	_, err := c.Resets(ctx, time.Time{})
	return err
}

func TestStatusActualShape(t *testing.T) {
	c := testClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, "/api/v1/status")
		if r.URL.RawQuery != "" {
			t.Error("status must not send query parameters")
		}
		_, _ = io.WriteString(w, sampleStatus)
	})
	status, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Valid() || status.LatestReset == nil || status.LatestReset.ID != "2110618732666257557" || status.LatestReset.ResetType != "regular" {
		t.Fatalf("invalid decoded latest reset: %+v", status.LatestReset)
	}
	if status.LatestReset.Source.Type != "x_post" || status.LatestReset.Source.Author != "thsottiaux" || status.LatestReset.Source.URL != "https://x.com/thsottiaux/status/2110618732666257557" {
		t.Fatalf("incorrect decoded source: %+v", status.LatestReset.Source)
	}
	if status.ScheduledReset != nil || status.ActiveWatch != nil {
		t.Fatal("explicit nulls must remain nil")
	}
	if status.Stats.Total != 62 || status.Stats.LastResetAt == nil || !status.Stats.LastResetAt.Equal(status.LatestReset.AnnouncedAt) || status.Stats.DaysSinceLast == nil || *status.Stats.DaysSinceLast != 0.1 || status.Stats.AvgIntervalDays == nil || *status.Stats.AvgIntervalDays != 6.4 {
		t.Fatalf("incorrect decoded stats: %+v", status.Stats)
	}
	if status.GeneratedAt.Format(time.RFC3339Nano) != "2026-10-15T09:37:51.309Z" {
		t.Fatalf("generated_at not copied from meta: %v", status.GeneratedAt)
	}
}

func TestStatusNullsAndForecastShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		forecasts bool
		nullDates bool
	}{
		{name: "no history", body: statusJSON("null", "null", "null", emptyStats)},
		{name: "scheduled and watch", body: statusJSON(sampleReset, sampleSchedule, sampleWatch, sampleStats), forecasts: true},
		{name: "nullable forecast fields", body: statusJSON(sampleReset, strings.Replace(sampleSchedule, `"scheduled_for":"2026-10-16T00:00:00Z"`, `"scheduled_for":null`, 1), strings.Replace(sampleWatch, `"reset_chance_percent":65`, `"reset_chance_percent":null`, 1), sampleStats), forecasts: true, nullDates: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			status, err := c.Status(context.Background())
			if err != nil || !status.Valid() {
				t.Fatalf("Status() failed: %v", err)
			}
			if !tc.forecasts {
				if status.LatestReset != nil || status.ScheduledReset != nil || status.ActiveWatch != nil || status.Stats.Total != 0 || status.Stats.LastResetAt != nil || status.Stats.DaysSinceLast != nil || status.Stats.AvgIntervalDays != nil {
					t.Fatal("empty status must preserve all nullable fields")
				}
				return
			}
			if status.ScheduledReset == nil || status.ActiveWatch == nil || status.ScheduledReset.Status != "scheduled" || status.ScheduledReset.ResetType != "banked" || status.ActiveWatch.Level != "elevated" || status.ActiveWatch.ForecastWindow != "next 24 hours" {
				t.Fatal("scheduled reset or active watch was not decoded")
			}
			if tc.nullDates {
				if status.ScheduledReset.ScheduledFor != nil || status.ActiveWatch.ResetChancePercent != nil {
					t.Fatal("nullable forecast fields did not stay nil")
				}
			} else if status.ScheduledReset.ScheduledFor == nil || status.ScheduledReset.ScheduledFor.Format(time.RFC3339) != "2026-10-16T00:00:00Z" || status.ActiveWatch.ResetChancePercent == nil || *status.ActiveWatch.ResetChancePercent != 65 {
				t.Fatal("forecast values were not decoded")
			}
		})
	}
}

func TestResetsFromPaginationAndObservedSource(t *testing.T) {
	from := time.Date(2026, 9, 1, 2, 3, 4, 123456789, time.FixedZone("offset", 5*60*60+30*60))
	var calls atomic.Int32
	c := testClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, "/api/v1/resets")
		q := r.URL.Query()
		if q.Get("limit") != "100" || q.Get("order") != "asc" || q.Get("from") != "2026-09-01T02:03:04.123456789+05:30" || q.Has("to") {
			t.Errorf("incorrect pagination query: %v", q)
		}
		switch calls.Add(1) {
		case 1:
			if q.Has("cursor") {
				t.Error("first request must omit cursor")
			}
			_, _ = io.WriteString(w, pageJSON("["+observedReset+"]", true, `"opaque_next-2"`))
		case 2:
			if q.Get("cursor") != "opaque_next-2" {
				t.Error("next cursor was not preserved")
			}
			_, _ = io.WriteString(w, pageJSON("["+sampleReset+"]", false, "null"))
		default:
			t.Error("unexpected additional page request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	resets, err := c.Resets(context.Background(), from)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(resets) != 2 || resets[0].ID != "observed-20260929T190000Z" || resets[1].ID != "2110618732666257557" {
		t.Fatalf("incorrect concatenated pages: %+v", resets)
	}
	if resets[0].Source.Type != "observed" || resets[0].Source.Author != "" || resets[0].ResetType != "banked" || !resets[0].Valid() || !resets[0].AnnouncedAt.Before(resets[1].AnnouncedAt) {
		t.Fatal("observed reset was not decoded correctly")
	}
}

func TestResetsZeroFromAndEmptyHistory(t *testing.T) {
	c := testClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("from") || r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("order") != "asc" {
			t.Error("all-history request must omit from and use ascending, full pages")
		}
		_, _ = io.WriteString(w, pageJSON("[]", false, "null"))
	})
	resets, err := c.Resets(context.Background(), time.Time{})
	if err != nil || len(resets) != 0 {
		t.Fatalf("empty history failed: %v, %v", resets, err)
	}
}

func TestObservedSourceWithoutURL(t *testing.T) {
	reset := strings.Replace(observedReset, `,"url":"https://x.com/thsottiaux/status/2105120226027450685"`, "", 1)
	c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, pageJSON("["+reset+"]", false, "null"))
	})
	resets, err := c.Resets(context.Background(), time.Time{})
	if err != nil || len(resets) != 1 || resets[0].Source.URL != "" {
		t.Fatalf("observed sources may omit URL: %v", err)
	}
}

func TestStatusRejectsMalformedAndMissingFields(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"null", "null"},
		{"empty object", "{}"},
		{"array", "[]"},
		{"truncated", `{"data":`},
		{"trailing json", sampleStatus + "{}"},
		{"invalid utf8", strings.Replace(sampleStatus, "Done", string([]byte{0xff}), 1)},
		{"missing meta", `{"data":{}}`},
		{"missing data", `{"meta":` + sampleMeta + `}`},
		{"null data", `{"data":null,"meta":` + sampleMeta + `}`},
		{"empty data", `{"data":{},"meta":` + sampleMeta + `}`},
		{"null meta", strings.Replace(sampleStatus, sampleMeta, "null", 1)},
		{"empty meta", strings.Replace(sampleStatus, sampleMeta, "{}", 1)},
		{"wrong version", strings.Replace(sampleStatus, `"v1"`, `"v2"`, 1)},
		{"missing generated at", strings.Replace(sampleStatus, `,"generated_at":"2026-10-15T09:37:51.309Z"`, "", 1)},
		{"zero generated at", strings.Replace(sampleStatus, "2026-10-15T09:37:51.309Z", "0001-01-01T00:00:00Z", 1)},
		{"bad generated at", strings.Replace(sampleStatus, "2026-10-15T09:37:51.309Z", "not-a-date", 1)},
		{"missing latest", strings.Replace(sampleStatus, `"latest_reset":`+sampleReset+`,`, "", 1)},
		{"missing schedule", strings.Replace(sampleStatus, `"scheduled_reset":null,`, "", 1)},
		{"missing watch", strings.Replace(sampleStatus, `"active_watch":null,`, "", 1)},
		{"null stats", statusJSON("null", "null", "null", "null")},
		{"empty stats", statusJSON("null", "null", "null", "{}")},
		{"null total", strings.Replace(sampleStatus, `"total":62`, `"total":null`, 1)},
		{"negative total", strings.Replace(sampleStatus, `"total":62`, `"total":-1`, 1)},
		{"fractional total", strings.Replace(sampleStatus, `"total":62`, `"total":1.5`, 1)},
		{"missing nullable stats", strings.Replace(sampleStatus, `,"avg_interval_days":6.4`, "", 1)},
		{"negative days", strings.Replace(sampleStatus, `"days_since_last":0.1`, `"days_since_last":-1`, 1)},
		{"negative interval", strings.Replace(sampleStatus, `"avg_interval_days":6.4`, `"avg_interval_days":-1`, 1)},
		{"zero last reset", strings.Replace(sampleStatus, `"last_reset_at":"2026-10-15T06:28:13.000Z"`, `"last_reset_at":"0001-01-01T00:00:00Z"`, 1)},
		{"empty latest", statusJSON("{}", "null", "null", sampleStats)},
		{"empty schedule", statusJSON(sampleReset, "{}", "null", sampleStats)},
		{"empty watch", statusJSON(sampleReset, "null", "{}", sampleStats)},
		{"missing scheduled for", statusJSON(sampleReset, strings.Replace(sampleSchedule, `"scheduled_for":"2026-10-16T00:00:00Z",`, "", 1), "null", sampleStats)},
		{"zero scheduled for", statusJSON(sampleReset, strings.Replace(sampleSchedule, "2026-10-16T00:00:00Z", "0001-01-01T00:00:00Z", 1), "null", sampleStats)},
		{"invalid schedule status", statusJSON(sampleReset, strings.Replace(sampleSchedule, `"status":"scheduled"`, `"status":"done"`, 1), "null", sampleStats)},
		{"invalid watch level", statusJSON(sampleReset, "null", strings.Replace(sampleWatch, `"elevated"`, `"certain"`, 1), sampleStats)},
		{"invalid watch chance", statusJSON(sampleReset, "null", strings.Replace(sampleWatch, `:65`, `:101`, 1), sampleStats)},
		{"missing watch chance", statusJSON(sampleReset, "null", strings.Replace(sampleWatch, `"reset_chance_percent":65,`, "", 1), sampleStats)},
		{"zero observed at", statusJSON(sampleReset, "null", strings.Replace(sampleWatch, "2026-10-15T06:28:13Z", "0001-01-01T00:00:00Z", 1), sampleStats)},
		{"zero expires at", statusJSON(sampleReset, "null", strings.Replace(sampleWatch, "2026-10-16T06:28:13Z", "0001-01-01T00:00:00Z", 1), sampleStats)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			status, err := c.Status(context.Background())
			clientError(t, err, "invalid_response")
			if status.LatestReset != nil || !status.GeneratedAt.IsZero() {
				t.Fatal("invalid status returned partial data")
			}
		})
	}
}

func TestResetsRejectsMalformedData(t *testing.T) {
	good := pageJSON("["+sampleReset+"]", false, "null")
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"empty object", "{}"},
		{"null", "null"},
		{"trailing json", good + "{}"},
		{"null data", pageJSON("null", false, "null")},
		{"object data", pageJSON("{}", false, "null")},
		{"missing data", `{"pagination":{"has_more":false,"next_cursor":null},"meta":` + sampleMeta + `}`},
		{"wrong version", strings.Replace(good, `"v1"`, `"v0"`, 1)},
		{"zero generated at", strings.Replace(good, "2026-10-15T09:37:51.309Z", "0001-01-01T00:00:00Z", 1)},
		{"missing meta", `{"data":[],"pagination":{"has_more":false,"next_cursor":null}}`},
		{"null reset", pageJSON("[null]", false, "null")},
		{"empty reset", pageJSON("[{}]", false, "null")},
		{"empty id", strings.Replace(good, `"id":"2110618732666257557"`, `"id":""`, 1)},
		{"bad reset type", strings.Replace(good, `"regular"`, `"unknown"`, 1)},
		{"zero announced at", strings.Replace(good, "2026-10-15T06:28:13.000Z", "0001-01-01T00:00:00Z", 1)},
		{"bad announced at", strings.Replace(good, "2026-10-15T06:28:13.000Z", "2026-10-15", 1)},
		{"missing text", strings.Replace(good, `"text":"Done and dusted https://t.co/0fehr43VS4",`, "", 1)},
		{"null text", strings.Replace(good, `"text":"Done and dusted https://t.co/0fehr43VS4"`, `"text":null`, 1)},
		{"null source", strings.Replace(good, sampleSource, "null", 1)},
		{"empty source", strings.Replace(good, sampleSource, "{}", 1)},
		{"missing author", strings.Replace(good, `"author":"thsottiaux",`, "", 1)},
		{"null author", strings.Replace(good, `"author":"thsottiaux"`, `"author":null`, 1)},
		{"wrong author", strings.Replace(good, `"author":"thsottiaux"`, `"author":"someone_else"`, 1)},
		{"invalid source URL", strings.Replace(good, `https://x.com/thsottiaux/status/2110618732666257557`, `not-a-url`, 1)},
		{"too many rows", pageJSON("["+strings.TrimSuffix(strings.Repeat(sampleReset+",", pageSize+1), ",")+"]", false, "null")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			resets, err := c.Resets(context.Background(), time.Time{})
			clientError(t, err, "invalid_response")
			if resets != nil {
				t.Fatal("invalid history returned partial data")
			}
		})
	}
}

func TestPaginationShapeAndCursorErrors(t *testing.T) {
	for _, tc := range []struct{ name, pagination, code string }{
		{"null pagination", "null", "invalid_response"},
		{"empty pagination", "{}", "invalid_response"},
		{"missing has more", `{"next_cursor":null}`, "invalid_response"},
		{"null has more", `{"has_more":null,"next_cursor":null}`, "invalid_response"},
		{"wrong has more type", `{"has_more":"false","next_cursor":null}`, "invalid_response"},
		{"missing cursor", `{"has_more":false}`, "invalid_response"},
		{"wrong cursor type", `{"has_more":true,"next_cursor":42}`, "invalid_response"},
		{"null cursor", `{"has_more":true,"next_cursor":null}`, "invalid_cursor"},
		{"empty cursor", `{"has_more":true,"next_cursor":""}`, "invalid_cursor"},
		{"blank cursor", `{"has_more":true,"next_cursor":" \t"}`, "invalid_cursor"},
		{"oversized cursor", `{"has_more":true,"next_cursor":"` + strings.Repeat("a", maxCursorBytes+1) + `"}`, "invalid_cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, `{"data":[],"pagination":`+tc.pagination+`,"meta":`+sampleMeta+`}`)
			})
			_, err := c.Resets(context.Background(), time.Time{})
			clientError(t, err, tc.code)
			if calls.Load() != 1 {
				t.Fatal("invalid pagination must stop without retrying")
			}
		})
	}
}

func TestRepeatedCursors(t *testing.T) {
	for _, cursors := range [][]string{{"same", "same"}, {"first", "second", "first"}} {
		t.Run(strings.Join(cursors, "-"), func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
				n := int(calls.Add(1)) - 1
				if n >= len(cursors) {
					t.Error("cursor cycle was followed")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = io.WriteString(w, pageJSON("["+sampleReset+"]", true, strconv.Quote(cursors[n])))
			})
			resets, err := c.Resets(context.Background(), time.Time{})
			clientError(t, err, "cursor_repeated")
			if resets != nil || int(calls.Load()) != len(cursors) {
				t.Fatal("cursor cycles must fail without partial history")
			}
		})
	}
}

func TestPaginationPageLimit(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(strconv.FormatBool(complete), func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, 5*time.Second, func(w http.ResponseWriter, _ *http.Request) {
				n := int(calls.Add(1))
				more := !(complete && n == maxPages)
				_, _ = io.WriteString(w, pageJSON("[]", more, strconv.Quote(strconv.Itoa(n))))
			})
			resets, err := c.Resets(context.Background(), time.Time{})
			if complete {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				clientError(t, err, "page_limit")
				if resets != nil {
					t.Fatal("page limit must not return truncated history")
				}
			}
			if int(calls.Load()) != maxPages {
				t.Fatalf("got %d requests, want %d", calls.Load(), maxPages)
			}
		})
	}
}

func TestLaterPageFailureDiscardsPartialHistory(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, pageJSON("["+observedReset+"]", true, `"next"`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "private upstream problem")
	})
	resets, err := c.Resets(context.Background(), time.Time{})
	clientError(t, err, "http_503")
	if resets != nil || calls.Load() != 2 {
		t.Fatal("failed page must not yield partial history or trigger internal retries")
	}
}

func TestHTTPErrorsAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for _, method := range []string{"status", "resets"} {
		for _, tc := range []struct {
			name   string
			status int
			header []string
			wait   time.Duration
		}{
			{"seconds", 429, []string{" 17 "}, 17 * time.Second},
			{"date", 429, []string{now.Add(2 * time.Minute).Format(http.TimeFormat)}, 2 * time.Minute},
			{"past date", 429, []string{now.Add(-time.Minute).Format(http.TimeFormat)}, 0},
			{"zero", 429, []string{"0"}, 0},
			{"negative", 429, []string{"-10"}, 0},
			{"fraction", 429, []string{"1.5"}, 0},
			{"invalid", 429, []string{"private-value"}, 0},
			{"absent", 429, nil, 0},
			{"multiple", 429, []string{"10", "25"}, 25 * time.Second},
			{"overflow", 429, []string{strings.Repeat("9", 40)}, time.Duration(1<<63 - 1)},
			{"overflow with garbage", 429, []string{strings.Repeat("9", 40) + "x"}, 0},
			{"unavailable", 503, []string{"30"}, 30 * time.Second},
			{"bad request", 400, nil, 0},
			{"not found", 404, nil, 0},
			{"unexpected empty success", 204, nil, 0},
			{"unexpected not modified", 304, nil, 0},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				c := testClient(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					for _, value := range tc.header {
						w.Header().Add("Retry-After", value)
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "not JSON: https://private.invalid/secret-path")
				})
				c.now = func() time.Time { return now }
				got := clientError(t, invoke(c, context.Background(), method), fmt.Sprintf("http_%d", tc.status))
				if got.RetryAfter != tc.wait || calls.Load() != 1 {
					t.Fatalf("RetryAfter = %v, want %v; requests = %d", got.RetryAfter, tc.wait, calls.Load())
				}
			})
		}
	}
}

func TestTimeoutIncludesReadingBody(t *testing.T) {
	for _, method := range []string{"status", "resets"} {
		for _, flush := range []bool{false, true} {
			t.Run(method+"/flush="+strconv.FormatBool(flush), func(t *testing.T) {
				c := testClient(t, 60*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
					if flush {
						_, _ = io.WriteString(w, `{"data":`)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				})
				err := invoke(c, context.Background(), method)
				clientError(t, err, "timeout")
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("timeout must support errors.Is(context.DeadlineExceeded)")
				}
			})
		}
	}
}

func TestContextCancellation(t *testing.T) {
	for _, method := range []string{"status", "resets"} {
		for _, before := range []bool{false, true} {
			t.Run(method+"/before="+strconv.FormatBool(before), func(t *testing.T) {
				started := make(chan struct{})
				c := testClient(t, 2*time.Second, func(w http.ResponseWriter, r *http.Request) {
					close(started)
					_, _ = io.WriteString(w, `{"data":`)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if before {
					cancel()
				}
				result := make(chan error, 1)
				go func() { result <- invoke(c, ctx, method) }()
				if !before {
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatal("request did not reach test server")
					}
					cancel()
				}
				select {
				case err := <-result:
					clientError(t, err, "canceled")
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancellation must support errors.Is(context.Canceled)")
					}
				case <-time.After(time.Second):
					t.Fatal("cancellation did not interrupt request")
				}
				if before {
					select {
					case <-started:
						t.Fatal("pre-canceled context made a request")
					default:
					}
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFullMethodTimeoutBudgetAcrossPages(t *testing.T) {
	c := New(80 * time.Millisecond)
	var deadlines []time.Time
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("request has no deadline")
		}
		deadlines = append(deadlines, deadline)
		if len(deadlines) == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(pageJSON("[]", true, `"next"`)))}, nil
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	resets, err := c.Resets(context.Background(), time.Time{})
	clientError(t, err, "timeout")
	if resets != nil || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatal("pagination must share one deadline and never return partial history on timeout")
	}
}

func TestCallerDeadlineWins(t *testing.T) {
	c := New(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("expired caller context reached transport")
		return nil, errors.New("private transport details")
	})
	for _, method := range []string{"status", "resets"} {
		clientError(t, invoke(c, ctx, method), "timeout")
	}
}

func TestResponseByteLimit(t *testing.T) {
	for _, method := range []string{"status", "resets"} {
		for _, chunked := range []bool{false, true} {
			for _, extra := range []int{0, 1} {
				t.Run(fmt.Sprintf("%s/chunked=%v/extra=%d", method, chunked, extra), func(t *testing.T) {
					base := sampleStatus
					if method == "resets" {
						base = pageJSON("[]", false, "null")
					}
					body := base + strings.Repeat(" ", maxResponseBytes-len(base)+extra)
					c := testClient(t, 5*time.Second, func(w http.ResponseWriter, _ *http.Request) {
						if chunked {
							w.WriteHeader(http.StatusOK)
							w.(http.Flusher).Flush()
						} else {
							w.Header().Set("Content-Length", strconv.Itoa(len(body)))
						}
						_, _ = io.WriteString(w, body)
					})
					err := invoke(c, context.Background(), method)
					if extra > 0 {
						clientError(t, err, "response_too_large")
					} else if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestRedirectDenied(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, sampleStatus)
	}))
	defer target.Close()
	c := testClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private-token", http.StatusFound)
	})
	for _, method := range []string{"status", "resets"} {
		clientError(t, invoke(c, context.Background(), method), "http_302")
	}
	if redirected.Load() != 0 {
		t.Fatal("client followed a redirect")
	}
}

func TestProductionTransportIsDirectAndIndependent(t *testing.T) {
	c := New(3 * time.Second)
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || c.httpClient.Transport == http.DefaultTransport || c.httpClient == http.DefaultClient || c.baseURL != productionBaseURL || c.timeout != 3*time.Second {
		t.Fatal("production client must own a bounded, direct transport")
	}
	if c.httpClient.CheckRedirect == nil || c.httpClient.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("production client must deny redirects")
	}
	if New(0).timeout <= 0 || New(-time.Second).timeout <= 0 {
		t.Fatal("nonpositive timeout must retain a bounded default")
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) {
	return 0, errors.New("private upstream URL and body")
}

func TestTransportAndReadErrorsAreSanitized(t *testing.T) {
	for _, method := range []string{"status", "resets"} {
		c := New(time.Second)
		c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("https://private.invalid/secret-path network failure")
		})
		clientError(t, invoke(c, context.Background(), method), "network")
		c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(failedReader{})}, nil
		})
		clientError(t, invoke(c, context.Background(), method), "response_read")
		c.baseURL = "https://private.invalid/%broken-secret"
		clientError(t, invoke(c, context.Background(), method), "invalid_request")
	}
	for _, code := range []string{"https://private.invalid/secret-path", "http_999", "http_200_secret", strings.Repeat("secret", 100)} {
		if (&Error{Code: code}).Error() != "codexresets: request_error" {
			t.Fatal("caller-created error leaked an arbitrary code")
		}
	}
	var nilError *Error
	if nilError.Error() != "codexresets: request_error" || nilError.Is(context.Canceled) {
		t.Fatal("nil error methods must be safe")
	}
}
