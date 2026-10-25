package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const openAITestEnglish = "https://openai.com/index/technical-article/"
const openAITestJapanese = "https://openai.com/ja-JP/index/technical-article/"

func TestOpenAIArticleURLPreservesPath(t *testing.T) {
	for _, suffix := range []string{"", "/", "?source=rss", ".pdf"} {
		raw := "https://openai.com/index/article" + suffix
		if got := openAIEnglishURL(raw); got != raw {
			t.Fatalf("URLを推測で変更しました: %q -> %q", raw, got)
		}
	}
}

func TestOpenAIRedirectDeliveryAndDedup(t *testing.T) {
	for _, status := range []int{301, 302, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ai, decisions, summaries := testAI(t)
			r := testRunner(t, ai)
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("User-Agent") != browserSourceUserAgent {
					t.Fatal("リダイレクトでUAを失いました")
				}
				if req.URL.String() == strings.TrimSuffix(openAITestJapanese, "/") {
					return fetchResponse(req, status, "", map[string]string{"Location": openAITestJapanese}), nil
				}
				if req.URL.String() != openAITestJapanese {
					t.Fatalf("unexpected URL: %s", req.URL)
				}
				return fetchResponse(req, 200, japaneseHTML(openAITestEnglish), nil), nil
			})}
			posts := 0
			r.post = func(_, payload string) error {
				posts++
				if payload != "link\n"+openAITestJapanese+"\n" {
					t.Fatalf("最終URLを投稿していません: %q", payload)
				}
				return nil
			}
			item := openAITestItem()
			item.Link = strings.TrimSuffix(openAITestEnglish, "/")
			if err := processOpenAITestItem(r, item); err != nil {
				t.Fatal(err)
			}
			item.Link, item.GUID = openAITestEnglish, "different-guid"
			if err := processOpenAITestItem(r, item); err != nil {
				t.Fatal(err)
			}
			if posts != 1 || *decisions != 1 || *summaries != 0 {
				t.Fatal("転送前後で再投稿しました")
			}
			if !r.state.done([]string{hash(strings.TrimSuffix(openAITestEnglish, "/"))}) || !r.state.done([]string{hash(openAITestEnglish)}) {
				t.Fatal("転送前後の既読状態が不正")
			}
		})
	}
}

func japaneseHTML(canonical string) string {
	return `<html lang="ja"><head><title>技術記事</title><link rel="canonical" href="` + canonical + `"></head><body><nav>English 日本語</nav><article><h1>技術記事</h1>` + strings.Repeat(`<p>この技術記事では分散システムの内部実装と通信の仕組みを詳しく説明します。性能を測定し、設計上の制約と改善方法を比較しました。接続の再利用により処理時間を短縮し、安全性も維持します。</p>`, 5) + `</article></body></html>`
}

func openAITestItem() *gofeed.Item {
	return &gofeed.Item{Title: "Technical article", Link: openAITestEnglish, GUID: "openai-test", Description: "RSS_SUMMARY: 本文の紹介 TLS implementation and benchmarks"}
}

func processOpenAITestItem(r *runner, item *gofeed.Item) error {
	f := Feed{Name: "OpenAI", URL: openAIFeedURL, Mode: "select", OpenAIJapanese: true}
	return r.processFeedItem(context.Background(), f, item, item.Link, "", itemKeys(f, item))
}

func TestOpenAIJapaneseSelection(t *testing.T) {
	for _, kind := range []string{"japanese", "missing", "redirect", "english-body", "rejected", "other-feed", "option-disabled"} {
		t.Run(kind, func(t *testing.T) {
			ai, decisions, summaries := testAI(t)
			if kind == "rejected" {
				ai.threshold = 1.9
			}
			r := testRunner(t, ai)
			japaneseGets, englishGets, posts, checkpoints := 0, 0, 0, 0
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodHead {
					return fetchResponse(req, 200, "", nil), nil
				}
				if req.URL.String() == openAITestJapanese {
					japaneseGets++
					switch kind {
					case "missing":
						return fetchResponse(req, 404, "", nil), nil
					case "redirect":
						return fetchResponse(req, 302, "", map[string]string{"Location": openAITestEnglish}), nil
					case "english-body":
						body := strings.Replace(fixtureHTML("English", ""), "<html>", `<html lang="ja-JP">`, 1)
						return fetchResponse(req, 200, body, nil), nil
					default:
						return fetchResponse(req, 200, japaneseHTML(openAITestEnglish), nil), nil
					}
				}
				englishGets++
				return fetchResponse(req, 200, fixtureHTML("English", ""), nil), nil
			})}
			r.post = func(id, payload string) error {
				posts++
				if r.state.done([]string{id}) {
					t.Fatal("投稿前に既読になりました")
				}
				if kind == "japanese" {
					if payload != "link\n"+openAITestJapanese+"\n" {
						t.Fatalf("unexpected link payload: %q", payload)
					}
					files, _ := filepath.Glob(filepath.Join(r.output, "pending", "*"))
					if len(files) != 0 {
						t.Fatal("日本語記事にファイルを生成しました")
					}
				} else if _, err := os.Stat(payload); err != nil {
					t.Fatal("英語記事の要約ファイルがありません", err)
				}
				return nil
			}
			r.checkpoint = func() error {
				checkpoints++
				s, err := loadState(r.statePath)
				if err != nil || !s.done([]string{hash(openAITestEnglish)}) {
					t.Fatal("投稿後の状態保存が不正", err)
				}
				return nil
			}
			item := openAITestItem()
			var err error
			if kind == "other-feed" || kind == "option-disabled" {
				f := Feed{Name: "Other", URL: "https://example.com/rss", Mode: "select"}
				if kind == "option-disabled" {
					f.URL = openAIFeedURL
				}
				err = r.processFeedItem(context.Background(), f, item, item.Link, "", itemKeys(f, item))
			} else {
				err = processOpenAITestItem(r, item)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantDecisions, wantSummaries, wantPosts := 1, 1, 1
			switch kind {
			case "japanese":
				wantDecisions, wantSummaries = 1, 0
			case "rejected":
				wantDecisions, wantSummaries, wantPosts = 1, 0, 0
			case "other-feed":
				wantDecisions = 1
			}
			if *decisions != wantDecisions || *summaries != wantSummaries || posts != wantPosts || checkpoints != wantPosts {
				t.Fatalf("calls: decisions=%d summaries=%d posts=%d checkpoints=%d", *decisions, *summaries, posts, checkpoints)
			}
			if kind == "rejected" && (japaneseGets != 1 || englishGets != 0) || kind == "japanese" && englishGets != 0 || (kind == "other-feed" || kind == "option-disabled") && (japaneseGets != 0 || englishGets != 1) {
				t.Fatalf("不要な本文取得: ja=%d en=%d", japaneseGets, englishGets)
			}
			if kind == "missing" && r.state.Processed[hash(openAITestJapanese)] != (time.Time{}) {
				t.Fatal("日本語版の不在を既読として記録しました")
			}
		})
	}
}

func TestOpenAILinkDeliveryRetryAndDedup(t *testing.T) {
	ai, decisions, summaries := testAI(t)
	r := testRunner(t, ai)
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fetchResponse(req, 200, japaneseHTML(openAITestEnglish), nil), nil
	})}
	posts := 0
	r.post = func(_, _ string) error { posts++; return fmt.Errorf("Slack失敗") }
	if err := processOpenAITestItem(r, openAITestItem()); !errors.Is(err, errDelivery) {
		t.Fatal("投稿失敗が伝播しません", err)
	}
	if r.state.done([]string{hash(openAITestEnglish), hash(openAITestJapanese)}) {
		t.Fatal("失敗した記事を既読にしました")
	}
	// 次回実行と同じく状態をディスクから復元する。
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	s, err := loadState(r.statePath)
	if err != nil {
		t.Fatal(err)
	}
	r.state, r.fetchAttempted, r.claimed = s, nil, map[string]bool{}
	r.post = func(_, _ string) error { posts++; return nil }
	if err := processOpenAITestItem(r, openAITestItem()); err != nil {
		t.Fatal(err)
	}
	item := openAITestItem()
	item.Link, item.GUID = openAITestJapanese, "another-guid"
	if err := processOpenAITestItem(r, item); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || *decisions != 2 || *summaries != 0 || !r.state.done([]string{hash(openAITestEnglish)}) || !r.state.done([]string{hash(openAITestJapanese)}) {
		t.Fatal("再試行または重複排除が不正", posts, *decisions, *summaries)
	}
}

func TestOpenAIProbeRetryPolicy(t *testing.T) {
	for _, status := range []int{403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ai, decisions, summaries := testAI(t)
			r := testRunner(t, ai)
			now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
			r.now = func() time.Time { return now }
			calls := 0
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != openAITestJapanese {
					t.Fatal("英語本文へ早すぎるフォールバック")
				}
				return fetchResponse(req, status, "", nil), nil
			})}
			r.crawlbase = &crawlbaseClient{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return fetchResponse(req, 200, japaneseHTML(openAITestEnglish), map[string]string{"cb_status": "200", "original_status": "200", "url": openAITestJapanese}), nil
			})}, token: "test", limit: 5}
			posts := 0
			r.post = func(_, _ string) error { posts++; return nil }
			err := processOpenAITestItem(r, openAITestItem())
			if errors.Is(err, errDelivery) || posts != 0 || *summaries != 0 || r.state.done([]string{hash(openAITestEnglish), hash(openAITestJapanese)}) {
				t.Fatal("確認失敗で既読・投稿しました", err)
			}
			if status == 500 {
				return
			}
			if status == 429 {
				before := *decisions
				if err := processOpenAITestItem(r, openAITestItem()); err != nil || *decisions != before {
					t.Fatal("待機中に興味判定を繰り返しました", err)
				}
			}
			if calls == 0 || r.state.Fetch[hash(openAITestJapanese)].Method != fetchCrawlbase {
				t.Fatal("Crawlbaseへの切り替えがありません")
			}
			now = now.Add(2 * time.Hour)
			r.fetchAttempted = nil
			if err := processOpenAITestItem(r, openAITestItem()); err != nil {
				t.Fatal(err)
			}
			if posts != 1 || *summaries != 0 || r.crawlbase.used != 1 {
				t.Fatal("代替取得後の日本語投稿が不正")
			}
		})
	}
}

func TestOpenAIInterestInput(t *testing.T) {
	for _, japanese := range []bool{true, false} {
		t.Run(fmt.Sprint(japanese), func(t *testing.T) {
			ai, _, _ := testAI(t)
			japaneseChecked := false
			decisions := 0
			ai.client = openai.NewClient(option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				decisions++
				b, err := io.ReadAll(req.Body)
				if !japaneseChecked || err != nil || req.URL.Path != "/v1/decisions" {
					t.Fatalf("unexpected decision: %s, %v", b, err)
				}
				if japanese && (!strings.Contains(string(b), "RSS_SUMMARY") || strings.Contains(string(b), "この技術記事")) {
					t.Fatalf("日本語版の判定にRSS情報だけを使っていません: %s", b)
				}
				if !japanese && (strings.Contains(string(b), "RSS_SUMMARY") || !strings.Contains(string(b), "This implementation uses TLS")) {
					t.Fatalf("英語版の判定に記事本文を使っていません: %s", b)
				}
				return metadataTestResponse(req)
			})}))
			r := testRunner(t, ai)
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() == openAITestJapanese {
					japaneseChecked = true
					if japanese {
						return fetchResponse(req, 200, japaneseHTML(openAITestEnglish), nil), nil
					}
					return fetchResponse(req, 404, "", nil), nil
				}
				if japanese {
					t.Fatal("不要な英語本文取得")
				}
				return fetchResponse(req, 200, fixtureHTML("English", ""), nil), nil
			})}
			r.post = func(_, _ string) error { t.Fatal("不採用記事を投稿しました"); return nil }
			if err := processOpenAITestItem(r, openAITestItem()); err != nil {
				t.Fatal(err)
			}
			if decisions != 1 {
				t.Fatalf("興味判定回数: %d", decisions)
			}
		})
	}
}

// SDKと同じHTTP経路を使い、実サービスには接続しない。
func metadataTestResponse(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"answers":[{"type":"score","name":"interest","score":0.1}]}`)), Request: req}, nil
}

func TestOpenAICanonicalDedup(t *testing.T) {
	ai, _, summaries := testAI(t)
	r := testRunner(t, ai)
	canonical := "https://openai.com/index/canonical/"
	r.state.Processed[hash(canonical)] = time.Now()
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fetchResponse(req, 200, japaneseHTML(canonical), nil), nil
	})}
	r.post = func(_, _ string) error { t.Fatal("canonicalが既読の記事を再投稿しました"); return nil }
	if err := processOpenAITestItem(r, openAITestItem()); err != nil {
		t.Fatal(err)
	}
	if *summaries != 0 || !r.state.done([]string{hash(openAITestEnglish)}) || !r.state.done([]string{hash(openAITestJapanese)}) {
		t.Fatal("canonicalの既読状態を引き継いでいません")
	}
}

func TestLinkDeliveryStream(t *testing.T) {
	for _, reply := range []string{"posted\ncontinue\n", "failed\n"} {
		var out strings.Builder
		post, checkpoint := deliveryStream(strings.NewReader(reply), &out)
		payload := openAITestJapanese + "\n"
		err := post("article-id", "link\n"+payload)
		if (err != nil) != (reply == "failed\n") {
			t.Fatal("リンク投稿の応答が不正", err)
		}
		if err == nil {
			if err := checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
		want := "link article-id " + base64.StdEncoding.EncodeToString([]byte(payload)) + "\n"
		if !strings.HasPrefix(out.String(), want) {
			t.Fatalf("unexpected event: %q", out.String())
		}
	}
}

func TestShellLinkPayload(t *testing.T) {
	script, err := os.ReadFile("scripts/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "post_article() {")
	end := strings.Index(string(script)[start:], "\nwhile IFS=") + start
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dir := t.TempDir()
			mock := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$TEST_ARGS\"\ncat > \"$TEST_BODY\"\n"
			if fail {
				mock += "exit 1\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "notify_slack"), []byte(mock), 0700); err != nil {
				t.Fatal(err)
			}
			payload := openAITestJapanese + "\n"
			cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+string(script)[start:end]+"\npost_article")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "event=link test", "id=test", "link_payload="+base64.StdEncoding.EncodeToString([]byte(payload)), "TEST_ARGS="+filepath.Join(dir, "args"), "TEST_BODY="+filepath.Join(dir, "body"))
			if b, err := cmd.CombinedOutput(); (err != nil) != fail {
				t.Fatalf("投稿結果が不正: %s %v", b, err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "body"))
			if err != nil || string(body) != payload {
				t.Fatal("リンク本文が不正", string(body), err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil || !strings.Contains(string(args), "-snippet-type\ntext\n-filename\ntest.txt") {
				t.Fatal("プレーンテキスト投稿ではありません", string(args), err)
			}
		})
	}
}

func TestOpenAIProbeFailureDoesNotCompleteEnglish(t *testing.T) {
	ai, _, summaries := testAI(t)
	r := testRunner(t, ai)
	englishStatus := 500
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == openAITestJapanese {
			return fetchResponse(req, 404, "", nil), nil
		}
		return fetchResponse(req, englishStatus, fixtureHTML("English", ""), nil), nil
	})}
	posts := 0
	r.post = func(_, _ string) error { posts++; return nil }
	if err := processOpenAITestItem(r, openAITestItem()); err == nil {
		t.Fatal("英語版の一時障害が失われました")
	}
	if posts != 0 || *summaries != 0 || r.state.done([]string{hash(openAITestEnglish), hash(openAITestJapanese)}) {
		t.Fatal("日本語版の404で英語版も既読になりました")
	}
	englishStatus = 200
	r.fetchAttempted = nil
	if err := processOpenAITestItem(r, openAITestItem()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || *summaries != 1 {
		t.Fatal("英語版を再試行できません")
	}
}
