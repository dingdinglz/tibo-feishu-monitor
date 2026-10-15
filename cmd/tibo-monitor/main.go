package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tibo-monitor/internal/codexresets"
	"tibo-monitor/internal/config"
	"tibo-monitor/internal/feishu"
	"tibo-monitor/internal/lock"
	"tibo-monitor/internal/model"
	"tibo-monitor/internal/monitor"
	"tibo-monitor/internal/scrape"
	"tibo-monitor/internal/store"
	"tibo-monitor/internal/typesafe"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], log); err != nil {
		log.Error("command_failed", "error", err.Error())
		os.Exit(1)
	}
}

func execute(ctx context.Context, args []string, log *slog.Logger) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage: tibo-monitor <run|probe|status|test-send> -config config.yaml\n  run [-debug]  Monitor continuously; -debug resends the latest post when no new posts are found\n  probe     Read the selected source (X scan or Codex report); no DB changes or Feishu messages\n  status    Show persisted progress and pending deliveries\n  test-send -destination ID -confirm    Send ONE explicit test message")
		return nil
	}
	command := args[0]
	if command != "run" && command != "probe" && command != "status" && command != "test-send" {
		return errors.New("unknown command; use help")
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	path := fs.String("config", "config.yaml", "YAML configuration file")
	destination := fs.String("destination", "", "test-send destination ID")
	confirm := fs.Bool("confirm", false, "explicitly allow one test message")
	pages := fs.Int("pages", 0, "probe-only page budget override (at least 2)")
	debug := fs.Bool("debug", false, "run-only: resend the latest post as a DEBUG test when no new posts are found")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (*pages != 0 && command != "probe") || ((*confirm || *destination != "") && command != "test-send") || (*debug && command != "run") {
		return errors.New("flag is not supported for this command")
	}
	c, err := config.Load(*path, command == "probe" || command == "status")
	if err != nil {
		return err
	}
	if command == "test-send" {
		if !*confirm || *destination == "" {
			return errors.New("test-send requires -destination ID and -confirm; no message was sent")
		}
		for _, d := range c.Webhooks {
			if d.ID == *destination && d.IsEnabled() {
				message, err := feishu.FormatNotice("这是一条手动触发的 Tibo 监控测试卡片，不是新的 X 动态。\n" + time.Now().UTC().Format(time.RFC3339))
				if err != nil {
					return err
				}
				if err := feishu.New(c.Delivery.Timeout).Send(ctx, d, message); err != nil {
					return err
				}
				log.Info("test_message_sent", "destination", d.ID)
				return nil
			}
		}
		return errors.New("selected destination does not exist or is disabled")
	}
	if command == "status" {
		if _, err := os.Stat(c.Database); err != nil {
			return errors.New("monitor database does not exist or cannot be read")
		}
		s, err := store.OpenReadOnly(c.Database)
		if err != nil {
			return errors.New("cannot open monitor database")
		}
		defer s.Close()
		st, err := s.Status(ctx)
		if err != nil {
			return err
		}
		return printJSON(st)
	}
	if err := preflight(&c, log); err != nil {
		return err
	}
	if !c.UsesCodexResets() {
		accountLock, err := lock.Acquire(c.Scrape.AccountsDB + ".tibo-monitor.lock")
		if err != nil {
			return err
		}
		defer accountLock.Close()
	}
	fetcher := scrape.Python{Executable: c.Scrape.Python, Script: c.Scrape.Script}
	resetProvider := codexresets.New(c.CodexResets.Timeout)
	if command == "probe" {
		if c.UsesCodexResets() {
			if *pages != 0 {
				return errors.New("pages is only supported by the X source")
			}
			report, err := monitor.BuildCodexReport(ctx, resetProvider, time.Now().UTC(), c.DailyReport.Timezone)
			if err != nil {
				return err
			}
			return printJSON(struct {
				Source      string            `json:"source"`
				Attribution string            `json:"attribution"`
				Report      model.CodexReport `json:"report"`
			}{c.Source, model.CodexResetsURL, report})
		}
		if *pages != 0 {
			if *pages < 2 || *pages > 100 {
				return errors.New("pages must be 2..100")
			}
			c.Scrape.MaxPages = *pages
		}
		res, err := fetcher.Fetch(ctx, model.FetchRequest{
			AccountsDB: c.Scrape.AccountsDB, UserID: c.Target.UserID, Username: c.Target.Username,
			Proxy: c.Scrape.Proxy, Since: time.Now().UTC().Add(-c.Poll.CatchUpWindow),
			MaxPages: c.Scrape.MaxPages, TimeoutSeconds: c.Scrape.Timeout.Seconds(),
			WaitSeconds: c.Scrape.WaitTimeout.Seconds(), ContextLimit: c.Scrape.ContextLimit,
		})
		res.Cursor = "" // no pagination tokens in human-facing probe output
		if outputErr := printJSON(res); outputErr != nil {
			return outputErr
		}
		return err
	}
	stateLock, err := lock.Acquire(c.Database + ".lock")
	if err != nil {
		return err
	}
	defer stateLock.Close()
	s, err := store.Open(c.Database)
	if err != nil {
		return errors.New("cannot initialize monitor database")
	}
	defer s.Close()
	if err := s.SyncDestinations(ctx, c.Target.UserID, c.Webhooks); err != nil {
		return err
	}
	if err := s.SetSource(ctx, c.Source); err != nil {
		return err
	}
	upgraded, err := s.UpgradePendingPosts(ctx, feishu.Format)
	if err != nil {
		return err
	}
	if upgraded > 0 {
		log.Info("pending_posts_upgraded_to_cards", "deliveries", upgraded)
	}
	m := monitor.Monitor{Config: c, Store: s, Fetcher: fetcher, ResetProvider: resetProvider, Sender: feishu.New(c.Delivery.Timeout), Log: log, Debug: *debug}
	if !c.UsesCodexResets() {
		m.Assessor = typesafe.New(c.TypeSafe)
		if c.TypeSafe.APIKey == "" {
			log.Warn("typesafe_not_configured", "hint", "set TYPESAFE_API_KEY; reset probability will be shown as unavailable until configured")
		}
		if c.DailyReport.Enabled {
			log.Warn("daily_report_inactive", "reason", "daily reports require source: codex-resets")
		}
	}
	log.Info("monitor_started", "source", c.Source, "target", c.Target.Username, "interval", c.Poll.Interval, "jitter", "1s..30s", "debug", *debug)
	return m.Run(ctx)
}

func preflight(c *config.Config, log *slog.Logger) error {
	// Resolve symlinks before deriving lock paths; aliases must share ownership.
	if real, err := filepath.EvalSymlinks(c.Database); err == nil {
		c.Database = real
	}
	if c.UsesCodexResets() {
		return nil
	}
	if real, err := filepath.EvalSymlinks(c.Scrape.AccountsDB); err == nil {
		c.Scrape.AccountsDB = real
	}
	acc, err := os.Stat(c.Scrape.AccountsDB)
	if err != nil || !acc.Mode().IsRegular() {
		return errors.New("scrape.accounts_db must point to an existing twscrape account database")
	}
	if acc.Mode().Perm()&0077 != 0 {
		log.Warn("account_database_permissions", "recommendation", "chmod 600 the account database; permissions were not changed")
	}
	if state, err := os.Stat(c.Database); err == nil && os.SameFile(acc, state) {
		return errors.New("account and monitor databases must be different files")
	}
	if _, err := exec.LookPath(c.Scrape.Python); err != nil {
		return errors.New("scrape.python is not an executable Python interpreter")
	}
	f, err := os.Stat(c.Scrape.Script)
	if err != nil || !f.Mode().IsRegular() {
		return errors.New("scrape.script must point to scripts/fetch.py")
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
