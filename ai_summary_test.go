package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestTruncateSummaryBody(t *testing.T) {
	short := "短い記事の本文"
	if got := truncateSummaryBody(short); got != short {
		t.Fatalf("短い本文が変更されました: %q", got)
	}

	// 48,000バイト目が日本語の文字の途中になる場合もUTF-8を壊さない。
	long := strings.Repeat("a", summaryInputLimit-1) + "日本語" + strings.Repeat("b", summaryInputLimit)
	got := truncateSummaryBody(long)
	if len(got) != summaryInputLimit-1 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
		t.Fatalf("本文の切り詰めが不正です: bytes=%d, validUTF8=%t", len(got), utf8.ValidString(got))
	}
}

func TestSummarizeSingleRequest(t *testing.T) {
	long := strings.Repeat("a", summaryInputLimit-1) + "日本語" + strings.Repeat("b", summaryInputLimit)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("想定外のAPIパス: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls++
		var req struct {
			Input        string `json:"input"`
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("API入力を読み取れません: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		want := fmt.Sprintf("タイトル: 記事\n原文URL: https://example.com/article\n本文:\n%s", truncateSummaryBody(long))
		if req.Input != want {
			t.Errorf("要約入力が不正です: bytes=%d, expected=%d", len(req.Input), len(want))
		}
		if !strings.Contains(req.Instructions, "要約プロンプト") {
			t.Error("要約用の指示が渡されていません")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"- 概要\\n- 技術的な発見","annotations":[]}]}]}`)
	}))
	defer srv.Close()

	a := &AI{
		client:        openai.NewClient(option.WithBaseURL(srv.URL+"/"), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		summaryModel:  "gpt-6.1-sol",
		summaryPrompt: "要約プロンプト",
	}
	got, err := a.summarize(context.Background(), Article{Title: "記事", URL: "https://example.com/article", Markdown: long})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.HasPrefix(got, "- 概要") {
		t.Fatalf("要約結果またはAPI呼び出し回数が不正です: calls=%d summary=%q", calls, got)
	}
}
