package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Database string `yaml:"database"`
	Target   struct {
		UserID   string `yaml:"user_id"`
		Username string `yaml:"username"`
	} `yaml:"target"`
	Poll struct {
		Interval      time.Duration `yaml:"interval"`
		CatchUpWindow time.Duration `yaml:"catch_up_window"`
	} `yaml:"poll"`
	Scrape struct {
		Python       string        `yaml:"python"`
		Script       string        `yaml:"script"`
		AccountsDB   string        `yaml:"accounts_db"`
		Proxy        string        `yaml:"proxy"`
		Timeout      time.Duration `yaml:"timeout"`
		WaitTimeout  time.Duration `yaml:"wait_timeout"`
		MaxPages     int           `yaml:"max_pages"`
		ContextLimit int           `yaml:"context_limit"`
	} `yaml:"scrape"`
	Delivery struct {
		Timeout      time.Duration `yaml:"timeout"`
		RetryInitial time.Duration `yaml:"retry_initial"`
		RetryMax     time.Duration `yaml:"retry_max"`
		SendInterval time.Duration `yaml:"send_interval"`
	} `yaml:"delivery"`
	Alerts struct {
		FailureThreshold int           `yaml:"failure_threshold"`
		Cooldown         time.Duration `yaml:"cooldown"`
	} `yaml:"alerts"`
	Webhooks []Destination `yaml:"webhooks"`
}

type Destination struct {
	ID      string `yaml:"id"`
	Enabled *bool  `yaml:"enabled"`
	URL     string `yaml:"url"`
	Secret  string `yaml:"secret"`
	Prefix  string `yaml:"prefix"`
}

func (d Destination) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

func Defaults() Config {
	var c Config
	c.Database = "data/monitor.db"
	c.Target.UserID = "1953337039510003712"
	c.Target.Username = "thsottiaux"
	c.Poll.Interval = 3 * time.Minute
	c.Poll.CatchUpWindow = 24 * time.Hour
	c.Scrape.Python = ".venv/bin/python"
	c.Scrape.Script = "scripts/fetch.py"
	c.Scrape.Timeout = 75 * time.Second
	c.Scrape.WaitTimeout = 15 * time.Second
	c.Scrape.MaxPages = 5
	c.Scrape.ContextLimit = 4
	c.Delivery.Timeout = 10 * time.Second
	c.Delivery.RetryInitial = 30 * time.Second
	c.Delivery.RetryMax = 30 * time.Minute
	c.Delivery.SendInterval = time.Second
	c.Alerts.FailureThreshold = 3
	c.Alerts.Cooldown = 30 * time.Minute
	return c
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var numberPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

func expand(s string) (string, error) {
	var missing bool
	s = envPattern.ReplaceAllStringFunc(s, func(match string) string {
		v, ok := os.LookupEnv(match[2 : len(match)-1])
		if !ok {
			missing = true
		}
		return v
	})
	if missing {
		return "", errors.New("referenced environment variable is not set")
	}
	return s, nil
}

func resolve(base, s string, executable bool) (string, error) {
	s, err := expand(s)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot resolve home directory")
		}
		s = filepath.Join(home, s[2:])
	}
	if s == "" {
		return "", errors.New("path is empty")
	}
	if executable && !strings.ContainsAny(s, `/\`) {
		return s, nil
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(base, s)
	}
	return filepath.Clean(s), nil
}

// Load expands values after YAML decoding, so environment values cannot inject YAML.
// Probe mode intentionally ignores webhook credentials and makes no outgoing Feishu calls.
func Load(path string, probe bool) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("cannot open configuration file")
	}
	defer f.Close()
	dec := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, errors.New("invalid YAML configuration: check syntax, types and unknown keys")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain one YAML document")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return c, errors.New("cannot resolve config path")
	}
	base := filepath.Dir(abs)
	for _, p := range []struct {
		name       string
		ptr        *string
		executable bool
	}{
		{"database", &c.Database, false}, {"scrape.python", &c.Scrape.Python, true},
		{"scrape.script", &c.Scrape.Script, false}, {"scrape.accounts_db", &c.Scrape.AccountsDB, false},
	} {
		*p.ptr, err = resolve(base, *p.ptr, p.executable)
		if err != nil {
			return c, fmt.Errorf("%s: %w", p.name, err)
		}
	}
	c.Scrape.Proxy, err = expand(c.Scrape.Proxy)
	if err != nil {
		return c, fmt.Errorf("scrape.proxy: %w", err)
	}
	if c.Scrape.Proxy != "" {
		u, e := url.Parse(c.Scrape.Proxy)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return c, errors.New("scrape.proxy must be an HTTP(S) or SOCKS5 URL")
		}
	}
	if !numberPattern.MatchString(c.Target.UserID) || !usernamePattern.MatchString(c.Target.Username) {
		return c, errors.New("invalid target user_id or username")
	}
	if c.Database == c.Scrape.AccountsDB {
		return c, errors.New("monitor database must differ from twscrape account database")
	}
	if c.Poll.Interval < time.Minute || c.Poll.Interval > 24*time.Hour {
		return c, errors.New("poll.interval must be between 1m and 24h")
	}
	if c.Poll.CatchUpWindow < c.Poll.Interval || c.Poll.CatchUpWindow > 30*24*time.Hour {
		return c, errors.New("poll.catch_up_window must cover interval and be at most 720h")
	}
	if c.Scrape.Timeout < time.Second || c.Scrape.Timeout > 10*time.Minute || c.Scrape.WaitTimeout < 0 || c.Scrape.WaitTimeout >= c.Scrape.Timeout {
		return c, errors.New("invalid scrape timeout or wait_timeout")
	}
	if c.Scrape.MaxPages < 2 || c.Scrape.MaxPages > 100 || c.Scrape.ContextLimit < 0 || c.Scrape.ContextLimit > 20 {
		return c, errors.New("scrape.max_pages must be 2..100 and context_limit 0..20")
	}
	if c.Delivery.Timeout < time.Second || c.Delivery.Timeout > time.Minute || c.Delivery.RetryInitial < time.Second || c.Delivery.RetryMax < c.Delivery.RetryInitial || c.Delivery.RetryMax > 24*time.Hour || c.Delivery.SendInterval < time.Second || c.Delivery.SendInterval > time.Minute {
		return c, errors.New("invalid delivery timing (send_interval must be 1s..1m)")
	}
	if c.Alerts.FailureThreshold < 1 || c.Alerts.Cooldown < time.Minute {
		return c, errors.New("invalid alerts threshold or cooldown")
	}
	seen := map[string]bool{}
	urls := map[string]bool{}
	active := 0
	for i := range c.Webhooks {
		d := &c.Webhooks[i]
		if !idPattern.MatchString(d.ID) || seen[d.ID] {
			return c, errors.New("webhook ids must be unique, 1..64 letters/digits/underscore/hyphen")
		}
		seen[d.ID] = true
		if d.Prefix == "" {
			d.Prefix = "Tibo 更新"
		}
		if len(d.Prefix) > 512 {
			return c, errors.New("webhook prefix must be at most 512 UTF-8 bytes")
		}
		if !d.IsEnabled() || probe {
			continue
		}
		active++
		d.URL, err = expand(d.URL)
		if err != nil {
			return c, fmt.Errorf("webhook %s url: %w", d.ID, err)
		}
		d.Secret, err = expand(d.Secret)
		if err != nil {
			return c, fmt.Errorf("webhook %s secret: %w", d.ID, err)
		}
		u, e := url.Parse(d.URL)
		if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Host != "open.feishu.cn" && u.Host != "open.larksuite.com") ||
			!strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/") || strings.TrimPrefix(u.Path, "/open-apis/bot/v2/hook/") == "" {
			return c, fmt.Errorf("webhook %s: expected official HTTPS custom-bot webhook URL", d.ID)
		}
		if urls[d.URL] {
			return c, errors.New("duplicate enabled webhook URL would send duplicate notifications")
		}
		urls[d.URL] = true
	}
	if !probe && active == 0 {
		return c, errors.New("configure at least one enabled webhook")
	}
	return c, nil
}
