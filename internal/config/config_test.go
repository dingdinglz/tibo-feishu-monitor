package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadTest(t *testing.T, extra string, probe bool) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("scrape:\n  accounts_db: accounts.db\n"+extra), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(p, probe)
}

func TestDefaultsAndRelativePaths(t *testing.T) {
	c, err := loadTest(t, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll.Interval != 3*time.Minute || c.Poll.CatchUpWindow != 24*time.Hour {
		t.Fatal(c.Poll)
	}
	if !filepath.IsAbs(c.Scrape.Script) || filepath.Dir(c.Scrape.AccountsDB) != filepath.Dir(filepath.Dir(c.Database)) {
		t.Fatal("relative paths not rooted at config")
	}
}

func TestEnvIsExpandedAfterYAML(t *testing.T) {
	t.Setenv("TEST_WEBHOOK", "https://open.feishu.cn/open-apis/bot/v2/hook/test-token")
	t.Setenv("TEST_SECRET", "abc:\nwebhooks: []\n\"secret\"")
	c, err := loadTest(t, "webhooks:\n  - id: team\n    url: ${TEST_WEBHOOK}\n    secret: ${TEST_SECRET}\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Webhooks[0].Secret != os.Getenv("TEST_SECRET") {
		t.Fatal("environment mutated YAML")
	}
}

func TestInvalidConfigs(t *testing.T) {
	for _, extra := range []string{
		"poll:\n  interval: 0s\n", "poll:\n  interval: 30s\n", "unknown: secret\n",
		"webhooks:\n  - id: a\n    url: http://example.org/secret\n",
		"webhooks:\n  - id: a\n    url: https://open.feishu.cn.evil.test/open-apis/bot/v2/hook/secret\n",
		"webhooks:\n  - id: a\n    enabled: false\n  - id: a\n    enabled: false\n",
		"webhooks:\n  - id: a\n    url: ${TIBO_TEST_MISSING_ENV_521}\n",
		"webhooks: []\n", "---\nwebhooks: []\n",
	} {
		t.Run(extra, func(t *testing.T) {
			_, err := loadTest(t, extra, false)
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential leaked: %v", err)
			}
		})
	}
}

func TestProbeDoesNotNeedWebhookSecrets(t *testing.T) {
	_, err := loadTest(t, "webhooks:\n  - id: team\n    url: ${TIBO_TEST_MISSING_ENV_521}\n", true)
	if err != nil {
		t.Fatal(err)
	}
}
