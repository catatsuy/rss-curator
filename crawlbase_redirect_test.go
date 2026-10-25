package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCrawlbaseRetriesRedirectTarget(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, location := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/location=%v", status, location), func(t *testing.T) {
				start := "https://openai.com/ja-JP/index/article"
				final := start + "/"
				c := &crawlbaseClient{token: "test", limit: 5}
				c.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					target := req.URL.Query().Get("url")
					if target == start {
						headers := map[string]string{"cb_status": "200", "original_status": fmt.Sprint(status), "url": final}
						if location {
							headers["original_header_location"] = "/ja-JP/index/article/"
						}
						return fetchResponse(req, 200, "DO_NOT_SUMMARIZE_REDIRECT_BODY", headers), nil
					}
					if target != final {
						t.Fatalf("unexpected retry URL: %s", target)
					}
					return fetchResponse(req, 200, japaneseHTML(final), map[string]string{"cb_status": "200", "original_status": "200", "url": final}), nil
				})}
				a, result, err := c.fetch(context.Background(), start, "記事")
				if err != nil || result.Reason != "success" || a.finalURL != final || !hasJapaneseArticle(a, final) || c.used != 2 || strings.Contains(a.Markdown, "DO_NOT_SUMMARIZE") {
					t.Fatal("転送先の再取得に失敗", result, err, c.used)
				}
			})
		}
	}
}

func TestCrawlbaseRedirectBounds(t *testing.T) {
	for _, kind := range []string{"cycle", "invalid", "missing", "excluded", "budget", "forbidden"} {
		t.Run(kind, func(t *testing.T) {
			c := &crawlbaseClient{token: "test", limit: 5}
			if kind == "budget" {
				c.limit = 1
			}
			c.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if c.used == 2 && kind == "forbidden" {
					return fetchResponse(req, 520, "", map[string]string{"cb_status": "705", "original_status": "403"}), nil
				}
				next := "https://openai.com/index/article/"
				switch kind {
				case "cycle":
					next = "https://openai.com/index/article"
				case "invalid":
					next = "https://api.crawlbase.com/?token=PRIVATE"
				case "missing":
					next = ""
				case "excluded":
					next = "https://youtube.com/watch?v=test"
				}
				return fetchResponse(req, 200, "", map[string]string{"cb_status": "200", "original_status": "302", "url": next}), nil
			})}
			_, result, err := c.fetch(context.Background(), "https://openai.com/index/article", "記事")
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("転送失敗または秘密情報の扱いが不正", err)
			}
			if kind == "budget" && (!errors.Is(err, errFetchDeferred) || c.used != 1) {
				t.Fatal("利用上限を超えました", c.used, err)
			}
			if kind == "forbidden" && (result.Reason != "target_forbidden" || c.used != 2) {
				t.Fatal("転送先の403を失いました", result)
			}
			if kind != "forbidden" && c.used != 1 {
				t.Fatal("不正な転送先にリクエストしました", c.used)
			}
		})
	}
}

func TestCrawlbaseRedirectBudgetDoesNotCountAsFailure(t *testing.T) {
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		return fetchResponse(req, 200, "", map[string]string{"cb_status": "200", "original_status": "302", "url": "https://openai.com/index/article/"}), nil
	})
	r.crawlbase.limit = 1
	u := "https://openai.com/index/article"
	r.state.Fetch[hash(u)] = FetchState{Method: fetchCrawlbase, LastSeen: r.fetchTime()}
	if _, err := r.fetchArticle(context.Background(), u, "記事", ""); !errors.Is(err, errFetchDeferred) {
		t.Fatal(err)
	}
	f := r.state.Fetch[hash(u)]
	if f.Failures != 0 || f.CrawlbaseFailures != 0 || f.Stopped || r.state.done([]string{hash(u)}) {
		t.Fatal("利用上限を取得失敗に加えました", f)
	}
}
