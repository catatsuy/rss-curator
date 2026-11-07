package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const sourceTestRSS = `<rss version="2.0"><channel><title>Test</title><link>https://example.com/</link><description>Test</description><item><title>Article</title><link>https://example.com/article</link></item></channel></rss>`

func TestSourceUserAgents(t *testing.T) {
	for _, host := range []string{"openai.com", "medium.com", "example.com"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("ARTICLE_USER_AGENT", "")
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("User-Agent") != defaultArticleUserAgent {
					t.Error("通常経路のUAがホストで変わりました")
				}
				return fetchResponse(req, 200, fixtureHTML("記事", ""), nil), nil
			})}
			if _, err := fetchArticle(context.Background(), client, "https://"+host+"/article", "", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFeedFallbackOrder(t *testing.T) {
	for _, success := range []string{fetchHTTP, fetchJA4, fetchCrawlbase} {
		t.Run(success, func(t *testing.T) {
			var methods []string
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				method := fetchHTTP
				if req.URL.Hostname() == "api.crawlbase.com" {
					method = fetchCrawlbase
				} else if req.Header.Get("User-Agent") == browserSourceUserAgent {
					method = fetchJA4
				}
				methods = append(methods, method)
				if method != success {
					return fetchResponse(req, 403, "", nil), nil
				}
				if method == fetchCrawlbase {
					return goodCrawlResponse(req, sourceTestRSS), nil
				}
				return fetchResponse(req, 200, sourceTestRSS, nil), nil
			})
			feed, err := r.fetchFeed(context.Background(), "https://example.com/feed.xml")
			if err != nil || len(feed.Items) != 1 {
				t.Fatal(feed, err)
			}
			want := []string{fetchHTTP}
			if success != fetchHTTP {
				want = append(want, fetchJA4)
			}
			if success == fetchCrawlbase {
				want = append(want, fetchCrawlbase)
			}
			if strings.Join(methods, ",") != strings.Join(want, ",") {
				t.Fatal(methods, want)
			}
		})
	}
}

func TestSourceRedirectPolicy(t *testing.T) {
	calls := 0
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return fetchResponse(req, 302, "", map[string]string{"Location": "https://example.com/next"}), nil
	})}
	if _, err := fetchFeed(context.Background(), client, "https://example.com/feed"); err == nil || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestFeedFallbackRespectsLimits(t *testing.T) {
	for _, scenario := range []string{"dry_run", "budget", "429", "404", "bot_challenge"} {
		t.Run(scenario, func(t *testing.T) {
			calls, cb := 0, 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "api.crawlbase.com" {
					cb++
					return goodCrawlResponse(req, sourceTestRSS), nil
				}
				calls++
				status := 403
				if scenario == "429" {
					status = 429
				}
				if scenario == "404" {
					status = 404
				}
				if scenario == "bot_challenge" {
					return fetchResponse(req, 200, "<html></html>", map[string]string{"cf-mitigated": "challenge"}), nil
				}
				return fetchResponse(req, status, "", nil), nil
			})
			r.dry = scenario == "dry_run"
			if scenario == "budget" {
				r.crawlbase.limit = 0
			}
			feed, err := r.fetchFeed(context.Background(), "https://site.example/feed")
			if scenario == "bot_challenge" {
				if err != nil || feed == nil || cb != 1 || calls != 2 {
					t.Fatal(err, calls, cb)
				}
				return
			}
			want := 2
			if scenario == "429" || scenario == "404" {
				want = 1
			}
			if err == nil || calls != want || cb != 0 {
				t.Fatal(err, calls, cb)
			}
			if scenario == "429" {
				_, _ = r.fetchFeed(context.Background(), "https://site.example/other-feed")
				if calls != want {
					t.Fatal("待機中に同じホストを取得しました")
				}
			}
		})
	}
}

func TestSkipNormalHTTP(t *testing.T) {
	for _, host := range []string{"openai.com", "www.openai.com", "OPENAI.COM", "notopenai.com", "openai.com.example.com"} {
		for _, skip := range []bool{false, true} {
			for _, crawl := range []bool{false, true} {
				t.Run(host+"/skip="+strconv.FormatBool(skip)+"/crawl="+strconv.FormatBool(crawl), func(t *testing.T) {
					normal, browser, proxy := 0, 0, 0
					feed := false
					r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
						body := fixtureHTML("Article", "")
						if feed {
							body = sourceTestRSS
						}
						if req.URL.Hostname() == "api.crawlbase.com" {
							proxy++
							return goodCrawlResponse(req, body), nil
						}
						if req.Header.Get("User-Agent") == browserSourceUserAgent {
							browser++
							if crawl {
								return fetchResponse(req, 403, "", nil), nil
							}
						} else {
							normal++
							if skip {
								t.Fatal("skip_httpで通常HTTPを送りました")
							}
						}
						return fetchResponse(req, 200, body, nil), nil
					})
					r.skipHTTP = skip
					raw := "https://" + host + "/article"
					// 保存済みのhttp状態もJA4から再開する。
					r.state.Fetch[hash(strings.ReplaceAll(raw, "OPENAI.COM", "openai.com"))] = FetchState{Method: fetchHTTP}
					if _, err := r.fetchArticle(context.Background(), raw, "", ""); err != nil {
						t.Fatal(err)
					}
					feed = true
					if _, err := r.fetchFeed(context.Background(), "https://"+host+"/feed"); err != nil {
						t.Fatal(err)
					}
					if skip {
						wantProxy := 0
						if crawl {
							wantProxy = 2
						}
						if normal != 0 || browser != 3 || proxy != wantProxy {
							t.Fatal(normal, browser, proxy)
						}
					} else if normal != 3 || browser != 0 || proxy != 0 {
						t.Fatal(normal, browser, proxy)
					}
				})
			}
		}
	}

}

func TestSkipHTTPDoesNotLeakBetweenFeeds(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		browser := req.Header.Get("User-Agent") == browserSourceUserAgent
		if browser != (req.URL.Path == "/skip") {
			t.Errorf("フィードの取得設定が不正: %s UA=%s", req.URL.Path, req.Header.Get("User-Agent"))
		}
		requests = append(requests, req.URL.Path)
		fmt.Fprint(w, sourceTestRSS)
	}))
	defer server.Close()
	dir := t.TempDir()
	config := filepath.Join(dir, "feeds.json")
	mustWrite(t, config, fmt.Sprintf(`[{"name":"Skip","url":%q,"mode":"select","skip_http":true},{"name":"Normal","url":%q,"mode":"select"}]`, server.URL+"/skip", server.URL+"/normal"))
	if err := run(context.Background(), config, filepath.Join(dir, "state.json"), filepath.Join(dir, "out"), true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requests, ",") != "/skip,/normal" {
		t.Fatal(requests)
	}
}
