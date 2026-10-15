package model

import (
	"strings"
	"testing"
	"time"
)

func TestCodexResetIDsAreOpaque(t *testing.T) {
	r := CodexReset{ID: "observed-2026-10-15T06:28:13Z", ResetType: "regular", AnnouncedAt: time.Now(), Source: CodexSource{Type: "observed"}}
	if !r.Valid() {
		t.Fatal("rejected an opaque API ID")
	}
	for _, id := range []string{"", "\nunsafe", strings.Repeat("x", 65)} {
		r.ID = id
		if r.Valid() {
			t.Fatal("accepted invalid ID")
		}
	}
}

func TestCodexSourceLinksAreRestricted(t *testing.T) {
	for _, link := range []string{"javascript:alert(1)", "https://x.com.evil.invalid/a", "https://secret@x.com/a", "http://x.com/a"} {
		s := CodexSource{Type: "observed", URL: link}
		if s.Valid() || s.Link() != CodexResetsURL {
			t.Fatal("unsafe API source link accepted")
		}
	}
}
