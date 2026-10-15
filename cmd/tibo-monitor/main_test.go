package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/store"
)

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHelpAndInvalidArguments(t *testing.T) {
	if err := execute(context.Background(), []string{"help"}, logger()); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"unknown"}, {"probe", "extra"}, {"run", "-pages", "2"}, {"status", "-confirm"}, {"probe", "-debug"}, {"status", "-debug"}, {"test-send", "-debug"}} {
		if err := execute(context.Background(), args, logger()); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestRunAcceptsDebugFlag(t *testing.T) {
	err := execute(context.Background(), []string{"run", "-debug", "-config", filepath.Join(t.TempDir(), "missing.yaml")}, logger())
	if err == nil || err.Error() != "cannot open configuration file" {
		t.Fatal("debug flag was not accepted", err)
	}
}

func TestMissingConfirmationNeverSends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	text := "scrape:\n  accounts_db: accounts.db\nwebhooks:\n  - id: team\n    url: https://open.feishu.cn/open-apis/bot/v2/hook/DUMMY_TEST_SECRET\n"
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	err := execute(context.Background(), []string{"test-send", "-config", p, "-destination", "team"}, logger())
	if err == nil || !strings.Contains(err.Error(), "-confirm") || strings.Contains(err.Error(), "DUMMY_TEST_SECRET") {
		t.Fatal(err)
	}
}

func TestProbeDoesNotCreateMonitorDatabase(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"accounts.db": "not accessed by fake bridge",
		"fetch.sh":    "printf '%s\\n' '{\"version\":1,\"head_ok\":true,\"complete\":false,\"pages\":1,\"posts\":[]}'\n",
		"config.yaml": "database: monitor.db\nscrape:\n  python: /bin/sh\n  script: fetch.sh\n  accounts_db: accounts.db\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := execute(context.Background(), []string{"probe", "-config", filepath.Join(dir, "config.yaml"), "-pages", "2"}, logger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor.db")); !os.IsNotExist(err) {
		t.Fatal("probe mutated monitor database")
	}
}

func TestPreflightRejectsAccountStateAlias(t *testing.T) {
	dir := t.TempDir()
	acc := filepath.Join(dir, "accounts.db")
	alias := filepath.Join(dir, "alias.db")
	if err := os.WriteFile(acc, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(acc, alias); err != nil {
		t.Fatal(err)
	}
	c := config.Defaults()
	c.Scrape.AccountsDB = acc
	c.Database = alias
	if err := preflight(&c, logger()); err == nil {
		t.Fatal("accepted alias of account DB")
	}
}

func TestStatusUsesReadOnlyConnection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("database: monitor.db\nscrape:\n  accounts_db: unused.db\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), []string{"status", "-config", p}, logger()); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0640 {
		t.Fatal("status changed DB permissions", err)
	}
}
