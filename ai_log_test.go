package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestInterestDecisionLog(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	ai, _, _ := testAI(t)
	article := Article{Title: "PRIVATE_TITLE", URL: "https://user:PRIVATE_PASSWORD@example.com/article?token=PRIVATE_TOKEN#PRIVATE_FRAGMENT", Markdown: "本文 PRIVATE_BODY"}
	for _, threshold := range []float64{0.8, 1.6} {
		logs.Reset()
		ai.threshold = threshold
		accepted, err := ai.interest(context.Background(), article, "PRIVATE_INTRO")
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] != "興味判定" || record["score"] != 1.5 || record["threshold"] != threshold || record["accepted"] != accepted || accepted != (threshold <= 1.5) || record["model"] != ai.interestModel || record["url"] != "https://example.com/article" {
			t.Fatalf("unexpected decision log: %v", record)
		}
		for _, secret := range []string{"PRIVATE_", ai.interestPrompt, ai.summaryPrompt} {
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("private data in decision log: %s", secret)
			}
		}
	}
}
