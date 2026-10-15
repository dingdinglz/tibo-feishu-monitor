package scrape

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func TestValidateIDsAuthorsAndURLs(t *testing.T) {
	req := model.FetchRequest{UserID: "1953337039510003712", MaxPages: 5}
	r := model.FetchResult{Version: 1, HeadOK: true, Pages: 1, Posts: []model.Post{{ID: "2110618732666257557", AuthorID: req.UserID, Author: "thsottiaux", CreatedAt: time.Now(), Kind: "post", URL: "https://evil.invalid/credential"}}}
	if err := Validate(&r, req); err != nil {
		t.Fatal(err)
	}
	if r.Posts[0].URL != "https://x.com/thsottiaux/status/2110618732666257557" {
		t.Fatal("noncanonical URL")
	}
	r.Posts[0].AuthorID = "other"
	if err := Validate(&r, req); err == nil {
		t.Fatal("foreign author accepted")
	}
	r.Posts = nil
	r.Error = "https://example.com/SECRET"
	if err := Validate(&r, req); err == nil {
		t.Fatal("unsafe error accepted")
	}
}

func TestLatestDebugCandidateIsValidatedEvenOutsideWindow(t *testing.T) {
	req := model.FetchRequest{UserID: "1953337039510003712", MaxPages: 5, IncludeLatest: true}
	p := model.Post{ID: "2110618732666257557", AuthorID: req.UserID, Author: "thsottiaux", CreatedAt: time.Now().Add(-48 * time.Hour), Kind: "post", URL: "https://evil.invalid"}
	r := model.FetchResult{Version: 1, HeadOK: true, Pages: 1, Latest: &p}
	if err := Validate(&r, req); err != nil {
		t.Fatal(err)
	}
	if r.Latest.URL != "https://x.com/thsottiaux/status/2110618732666257557" {
		t.Fatal("latest URL not canonicalized")
	}
	p.AuthorID = "other"
	if err := Validate(&r, req); err == nil {
		t.Fatal("foreign debug author accepted")
	}
}

func TestBridgeProtocolAndSecretStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.sh")
	script := `#!/bin/sh
printf '%s\n' 'SECRET_COOKIE_AND_URL' >&2
printf '%s\n' '{"version":1,"head_ok":true,"complete":false,"pages":1,"posts":[],"error":"no_account"}'
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := (Python{Executable: "/bin/sh", Script: path}).Fetch(context.Background(), model.FetchRequest{MaxPages: 5, TimeoutSeconds: 1})
	if err == nil || err.Error() != "no_account" || !r.HeadOK {
		t.Fatal(r, err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("secret exposed")
	}
}

func TestInvalidJSONNeverMeansEmptySuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(path, []byte("printf 'not json SECRET'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := (Python{Executable: "/bin/sh", Script: path}).Fetch(context.Background(), model.FetchRequest{MaxPages: 5, TimeoutSeconds: 1})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

func TestCappedOutput(t *testing.T) {
	b := &cappedBuffer{max: 5}
	if _, err := b.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("6")); err == nil {
		t.Fatal("uncapped output")
	}
}
