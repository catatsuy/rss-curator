package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	readability "github.com/mackee/go-readability"
)

func TestStructuredArticleExtraction(t *testing.T) {
	for _, tc := range []struct{ fixture, url, want, absent string }{
		{"structured-article-body.html", "https://example.com/news/protocol-update", "Protocol version 2.1", "Newsletter signup"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			original, err := readability.Extract(string(body), readability.DefaultOptions())
			if err != nil || original.Root != nil {
				t.Fatal("既存抽出が失敗する構造を再現していません", err)
			}
			for _, method := range []string{fetchHTTP, fetchCrawlbase} {
				r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "api.crawlbase.com" {
						return goodCrawlResponse(req, string(body)), nil
					}
					return fetchResponse(req, 200, string(body), nil), nil
				})
				r.state.Fetch[hash(tc.url)] = FetchState{Method: method}
				a, err := r.fetchArticle(context.Background(), tc.url, "", "")
				if err != nil || !strings.Contains(a.Markdown, tc.want) || strings.Contains(a.Markdown, tc.absent) {
					t.Fatalf("%s: %v %s", method, err, a.Markdown)
				}
				if a.Title == "" {
					t.Fatal("タイトルがありません")
				}
			}
		})
	}
}

func TestStructuredArticleRejectsBoilerplate(t *testing.T) {
	for _, body := range []string{
		`<main><p>Access denied</p></main>`,
		`<main><nav><p>` + strings.Repeat("navigation ", 50) + `</p></nav></main>`,
		`<main><p><a href="/">` + strings.Repeat("link list ", 50) + `</a></p></main>`,
		`<div><p>` + strings.Repeat("Outside a declared article body. ", 50) + `</p></div>`,
		`<main hidden><p>` + strings.Repeat("Invisible content. ", 50) + `</p></main>`,
	} {
		doc, err := parseHTMLForTest(body)
		if err != nil {
			t.Fatal(err)
		}
		if _, md := structuredArticleContent(doc); md != "" {
			t.Fatal("本文以外を採用しました", md)
		}
	}
}

func TestShortLandingPageSkipsWithoutFallbackOrNotice(t *testing.T) {
	body, err := os.ReadFile("testdata/short-landing-page.html")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Hostname() == "api.crawlbase.com" || req.Header.Get("User-Agent") == browserSourceUserAgent {
			t.Fatal("本文なしに代替取得を行いました")
		}
		return fetchResponse(req, 200, string(body), nil), nil
	})
	raw := "https://example.com/"
	r.post = func(string, string) error { t.Fatal("本文なしをSlackに通知しました"); return nil }
	if err = r.process(context.Background(), "紹介", "select", raw, "Example Project", "", "", []string{hash(raw)}); err != nil {
		t.Fatal(err)
	}
	f := r.state.Fetch[hash(raw)]
	if f.StopReason != "no_article_content" || f.FailureNoticePending || f.Failures != 0 || !r.state.done([]string{hash(raw)}) {
		t.Fatal(f)
	}
	next := restartFetcher(t, r, 0)
	if err = next.process(context.Background(), "紹介", "select", raw, "Example Project", "", "", []string{hash(raw)}); err != nil || calls != 2 {
		t.Fatal("スキップ後に再取得しました", err, calls)
	}
}

func TestBotChallengeDetection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		headers    map[string]string
		blocked    bool
	}{
		{"cloudflare-header", fixtureHTML("Article", ""), map[string]string{"cf-mitigated": "challenge"}, true},
		{"proxied-header", fixtureHTML("Article", ""), map[string]string{"original_header_cf-mitigated": "challenge"}, true},
		{"cloudflare-html", `<html><head><title>Just a moment...</title></head><body><form id="challenge-form">Verify you are human</form></body></html>`, nil, true},
		{"challenge-script", `<html><head><title>Checking your browser</title></head><body><script src="/cdn-cgi/challenge-platform/test.js"></script></body></html>`, nil, true},
		{"article-about-bots", fixtureHTML("Just a moment...", `<p>Verify you are human. This article explains CAPTCHA and bot detection.</p><pre>cf-mitigated: challenge</pre>`), nil, false},
		{"embedded-turnstile", fixtureHTML("Article", `<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>`), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "https://site.example/article", nil)
			_, err := readArticleResponse(Article{}, fetchResponse(req, 200, tc.body, tc.headers))
			if errors.Is(err, errBotChallenge) != tc.blocked {
				t.Fatal("bot確認画面の判定が不正", err)
			}
			if !tc.blocked && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTP200BotChallengeFallsBack(t *testing.T) {
	for _, success := range []string{fetchJA4, fetchCrawlbase} {
		t.Run(success, func(t *testing.T) {
			calls := 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				method := fetchHTTP
				if req.URL.Hostname() == "api.crawlbase.com" {
					method = fetchCrawlbase
				} else if req.Header.Get("User-Agent") == browserSourceUserAgent {
					method = fetchJA4
				}
				if req.Method == "HEAD" {
					return fetchResponse(req, 200, "", nil), nil
				}
				calls++
				if method != success {
					return fetchResponse(req, 200, fixtureHTML("Blocked", ""), map[string]string{"cf-mitigated": "challenge"}), nil
				}
				if method == fetchCrawlbase {
					return goodCrawlResponse(req, fixtureHTML("Article", "")), nil
				}
				return fetchResponse(req, 200, fixtureHTML("Article", ""), nil), nil
			})
			raw := "https://site.example/article"
			a, err := r.fetchArticle(context.Background(), raw, "", "")
			want := 2
			if success == fetchCrawlbase {
				want = 3
			}
			if err != nil || calls != want || !strings.Contains(a.Markdown, "12ms") || r.state.done([]string{hash(raw)}) {
				t.Fatal(err, calls, a.Markdown)
			}
		})
	}
}
