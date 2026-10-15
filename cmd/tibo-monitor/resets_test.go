package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/store"
)

func TestCodexPreflightWithoutX(t *testing.T) {
	c := config.Defaults()
	c.Source = config.SourceCodexResets
	c.Database = filepath.Join(t.TempDir(), "monitor.db")
	if err := preflight(&c, logger()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRunNeedsNoPythonAccountOrAIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	text := "source: https://codex-resets.com/api/docs\ndatabase: monitor.db\nscrape:\n  accounts_db: ${UNSET_ACCOUNT_ENV}\ntypesafe:\n  api_key: ${UNSET_AI_KEY_ENV}\nwebhooks:\n  - id: team\n    url: https://open.feishu.cn/open-apis/bot/v2/hook/DUMMY_TEST\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no outgoing network calls
	if err := execute(ctx, []string{"probe", "-config", path, "-pages", "2"}, logger()); err == nil || !strings.Contains(err.Error(), "X source") {
		t.Fatal("unexpected API pages validation", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor.db")); !os.IsNotExist(err) {
		t.Fatal("API probe mutated monitor database")
	}
	// A cancelled startup may stop at a context-aware SQLite operation, but it
	// must never try to open or lock an X account database.
	err := execute(ctx, []string{"run", "-config", path}, logger())
	if err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor.db")); err != nil {
		t.Fatal("did not reach monitor storage without X", err)
	}
	s, err := store.OpenReadOnly(filepath.Join(dir, "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Status(context.Background())
	if err != nil || st.Pending != 0 {
		t.Fatal("cancelled startup queued messages", st, err)
	}
}
