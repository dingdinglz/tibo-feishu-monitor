package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"tibo-monitor/internal/model"
)

type Python struct{ Executable, Script string }

type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("output_limit")
	}
	return b.Buffer.Write(p)
}

var digits = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var username = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)
var code = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

func (p Python) Fetch(ctx context.Context, req model.FetchRequest) (model.FetchResult, error) {
	var result model.FetchResult
	input, err := json.Marshal(req)
	if err != nil {
		return result, errors.New("invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds*float64(time.Second))+10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Executable, p.Script)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard // upstream exceptions can contain account/proxy information
	out := &cappedBuffer{max: 16 << 20}
	cmd.Stdout = out
	cmd.Env = append(os.Environ(), "TWS_TELEMETRY=0", "DO_NOT_TRACK=1", "PYTHONUNBUFFERED=1", "PYTHONUTF8=1")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return result, errors.New("fetch_timeout_or_cancelled")
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return result, errors.New("bridge_failed_or_invalid_json")
	}
	if err := Validate(&result, req); err != nil {
		return model.FetchResult{}, err
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	if runErr != nil {
		result.Complete = false
		return result, errors.New("bridge_exit_failure")
	}
	if !result.HeadOK {
		return result, errors.New("empty_or_unavailable_timeline")
	}
	return result, nil
}

func Validate(r *model.FetchResult, req model.FetchRequest) error {
	invalid := errors.New("invalid_bridge_response")
	if r.Version != 1 || r.Pages < 0 || r.Pages > req.MaxPages+3 || len(r.Cursor) > 16384 || r.RetryAfterSeconds < 0 || r.RetryAfterSeconds > 7*24*3600 || len(r.Posts) > 20000 || (r.Error != "" && !code.MatchString(r.Error)) {
		return invalid
	}
	if r.Error != "" {
		r.Complete = false
	}
	for _, w := range r.Warnings {
		if !code.MatchString(w) {
			return invalid
		}
	}
	for i := range r.Posts {
		if !validPost(&r.Posts[i], req.UserID) {
			return invalid
		}
	}
	if r.Latest != nil && !validPost(r.Latest, req.UserID) {
		return invalid
	}
	if len(r.Contexts) > 20000 {
		return invalid
	}
	for i := range r.Contexts {
		if !validContext(&r.Contexts[i]) {
			return invalid
		}
	}
	return nil
}

func validPost(p *model.Post, userID string) bool {
	if !digits.MatchString(p.ID) || p.AuthorID != userID || !username.MatchString(p.Author) || p.CreatedAt.IsZero() || p.CreatedAt.Before(time.Unix(0, 0)) || p.CreatedAt.After(time.Now().Add(10*time.Minute)) || len(p.Text) > 1<<20 {
		return false
	}
	if p.Kind != "post" && p.Kind != "reply" && p.Kind != "quote" {
		return false
	}
	if p.ReplyToID != "" && !digits.MatchString(p.ReplyToID) {
		return false
	}
	p.URL = "https://x.com/" + p.Author + "/status/" + p.ID
	for _, c := range []*model.Context{p.Parent, p.Quote} {
		if c != nil && !validContext(c) {
			return false
		}
	}
	return true
}

func validContext(c *model.Context) bool {
	if !digits.MatchString(c.ID) || len(c.Text) > 1<<20 || (c.Author != "" && !username.MatchString(c.Author)) {
		return false
	}
	c.URL = "https://x.com/i/status/" + c.ID
	if c.Author != "" {
		c.URL = "https://x.com/" + c.Author + "/status/" + c.ID
	}
	return !strings.ContainsRune(c.Text, '\x00')
}
