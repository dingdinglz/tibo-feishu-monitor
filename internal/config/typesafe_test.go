package config

import (
	"strings"
	"testing"
	"time"
)

const testWebhook = "webhooks:\n  - id: team\n    url: https://open.feishu.cn/open-apis/bot/v2/hook/DUMMY_TEST_TOKEN\n"

func TestTypeSafeDefaultsAndEnvironmentKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "DUMMY_TEST_KEY")
	c, err := loadTest(t, testWebhook, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.TypeSafe.APIKey != "DUMMY_TEST_KEY" || c.TypeSafe.Model != "jev-latest" || c.TypeSafe.Timeout != 15*time.Second || c.TypeSafe.RecentPosts != 8 || c.TypeSafe.HistoryWindow != 72*time.Hour {
		t.Fatal("incorrect TypeSafe defaults")
	}
	t.Setenv("OTHER_TYPESAFE_TEST_KEY", "DUMMY_OTHER_KEY")
	c, err = loadTest(t, "typesafe:\n  api_key: ${OTHER_TYPESAFE_TEST_KEY}\n  model: jev-1.13.0\n  timeout: 5s\n  recent_posts: 3\n  history_window: 24h\n"+testWebhook, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.TypeSafe.APIKey != "DUMMY_OTHER_KEY" || c.TypeSafe.Model != "jev-1.13.0" || c.TypeSafe.RecentPosts != 3 {
		t.Fatal("TypeSafe explicit configuration ignored")
	}
	t.Setenv("TYPESAFE_API_KEY", "")
	c, err = loadTest(t, testWebhook, false)
	if err != nil || c.TypeSafe.APIKey != "" {
		t.Fatal("missing optional key must allow unavailable cards", err)
	}
}

func TestProbeAndStatusIgnoreTypeSafeKey(t *testing.T) {
	c, err := loadTest(t, "typesafe:\n  api_key: ${TIBO_TEST_MISSING_AI_KEY_521}\n", true)
	if err != nil || c.TypeSafe.APIKey != "" {
		t.Fatal("read-only commands should not require inference credentials", err)
	}
}

func TestTypeSafeBoundsAndSafeErrors(t *testing.T) {
	for _, field := range []string{
		"timeout: 0s", "timeout: 61s", "recent_posts: 0", "recent_posts: 11",
		"history_window: 30m", "history_window: 721h", "model: ''", "model: 'DO_NOT_LEAK/'",
		"api_key: ${TIBO_TEST_MISSING_AI_KEY_521}",
	} {
		_, err := loadTest(t, "typesafe:\n  "+field+"\n"+testWebhook, false)
		if err == nil || strings.Contains(err.Error(), "DO_NOT_LEAK") {
			t.Fatalf("unsafe config validation for %s: %v", field, err)
		}
	}
}
