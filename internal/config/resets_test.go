package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexSourceAliasesAndInactiveCredentials(t *testing.T) {
	for _, source := range []string{"codex-resets", "https://codex-resets.com/api/docs", "https://codex-resets.com/api/docs/", "https://codex-resets.com"} {
		t.Run(source, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			text := "source: " + source + "\nscrape:\n  python: ${UNSET_X_PYTHON}\n  accounts_db: ${UNSET_X_ACCOUNTS}\n  proxy: ${UNSET_X_PROXY}\ntypesafe:\n  api_key: ${UNSET_X_AI_KEY}\nwebhooks:\n  - id: team\n    url: https://open.feishu.cn/open-apis/bot/v2/hook/DUMMY_TEST\n"
			if err := os.WriteFile(p, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p, false)
			if err != nil || !c.UsesCodexResets() || c.TypeSafe.APIKey != "" {
				t.Fatal("API source still needs X credentials", err)
			}
			if c.DailyReport.Enabled || c.DailyReport.Time != "09:00" || c.DailyReport.Timezone != "Asia/Shanghai" || c.CodexResets.Timeout != 20*time.Second {
				t.Fatal("unexpected defaults", c.DailyReport)
			}
		})
	}
}

func TestCodexMinimalConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("source: codex-resets\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, true); err != nil {
		t.Fatal("API probe should need no accounts/webhook", err)
	}
}

func TestInvalidSourceAndSchedule(t *testing.T) {
	for _, text := range []string{
		"source: https://evil.invalid/api/docs\n",
		"source: codex-resets\ncodex_resets:\n  timeout: 0s\n",
		"source: codex-resets\ntarget:\n  username: other\n",
		"daily_report:\n  time: '24:00'\n",
		"daily_report:\n  time: '9:00'\n",
		"daily_report:\n  time: '09:60'\n",
		"daily_report:\n  timezone: ''\n",
		"daily_report:\n  timezone: Mars/Olympus\n",
	} {
		t.Run(text, func(t *testing.T) {
			if _, err := loadTest(t, text, true); err == nil {
				t.Fatal("accepted invalid source/schedule")
			}
		})
	}
	c, err := loadTest(t, "daily_report:\n  enabled: true\n  time: '08:30'\n  timezone: UTC\n", true)
	if err != nil || c.Source != SourceX || !c.DailyReport.Enabled {
		t.Fatal("X remains compatible; scheduling is inactive at runtime", err)
	}
}
