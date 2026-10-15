package feishu

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"tibo-monitor/internal/model"
)

func TestProbabilityBarForEveryPostTypeIncludingDebug(t *testing.T) {
	for _, kind := range []string{"post", "reply", "quote"} {
		for _, debug := range []bool{false, true} {
			for _, tc := range []struct {
				probability  float64
				bar, percent string
			}{
				{0, "░░░░░░░░░░", "0%"}, {math.Copysign(0, -1), "░░░░░░░░░░", "0%"}, {0.873, "█████████░", "87.3%"}, {1, "██████████", "100%"},
			} {
				p := examplePost()
				p.Kind = kind
				p.Text = strings.Repeat("正文🙂<at user_id=all>", 10000)
				p.Parent = &model.Context{ID: "1", Text: strings.Repeat("父帖", 500), Available: true}
				p.Quote = &model.Context{ID: "2", Text: "quoted", Available: true}
				messages, err := formatPost(p, debug)
				if err != nil {
					t.Fatal(err)
				}
				a := model.ResetAssessment{Probability: &tc.probability, Model: "jev-test", EvaluatedAt: time.Now(), HistoryCount: 3}
				text, err := WithResetAssessment(messages[0], a)
				if err != nil {
					t.Fatal(err)
				}
				d := destination()
				d.Prefix = strings.Repeat("\x00", 512)
				payload, wire := capturedCard(t, text, d)
				contents := allContent(payload.Card)
				for _, want := range []string{"本次额度重置概率", tc.bar + "  " + tc.percent, "TypeSafe", "jev-test", "此前 3 条", "并非重置保证", "原文过长，已截断"} {
					if !strings.Contains(contents, want) {
						t.Fatalf("missing %q in %s", want, contents)
					}
				}
				if len(wire) > maxCardRequestBytes || len(payload.Card.Body.Elements) != 5 {
					t.Fatal("card limits violated")
				}
				if debug && !strings.Contains(contents, "DEBUG 测试") {
					t.Fatal("debug marker lost")
				}
				assertOriginalButton(t, payload.Card, p.URL)
			}
		}
	}
}

func TestUnavailableOrInvalidAssessmentNeverLooksLikeZero(t *testing.T) {
	messages, err := Format(examplePost())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := capturedCard(t, messages[0], destination())
	contents := allContent(payload.Card)
	if !strings.Contains(contents, "暂不可用") || strings.Contains(contents, "%") || strings.Contains(contents, "░") {
		t.Fatal("missing judgment displayed as a probability", contents)
	}
	for _, value := range []float64{-0.1, 1.01, math.NaN(), math.Inf(1)} {
		a := model.ResetAssessment{Probability: &value, Model: "jev-test", EvaluatedAt: time.Now()}
		if _, err := WithResetAssessment(messages[0], a); err == nil {
			t.Fatal("invalid assessment accepted")
		}
	}
	// Even a malformed durable result cannot cause a negative Repeat or show a bogus percentage.
	var record cardRecord
	if err := json.Unmarshal([]byte(messages[0]), &record); err != nil {
		t.Fatal(err)
	}
	bad := 10.0
	record.Reset = &model.ResetAssessment{Probability: &bad, Model: "jev-test", EvaluatedAt: time.Now()}
	body, err := encodeCard(record)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = capturedCard(t, body, destination())
	if !strings.Contains(allContent(payload.Card), "暂不可用") {
		t.Fatal("invalid persisted result shown")
	}
	if CardPost("plain operational alert") != nil {
		t.Fatal("alert treated as a post")
	}
}
