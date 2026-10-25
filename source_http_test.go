package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const sourceTestRSS = `<rss version="2.0"><channel><title>Test</title><link>https://openai.com/</link><description>Test</description><item><title>Article</title><link>https://openai.com/index/article</link></item></channel></rss>`

func TestSourceUserAgents(t *testing.T) {
	for _, custom := range []string{"", "  MediumReader/1.0  "} {
		for _, raw := range []string{
			"https://openai.com/news/rss.xml", "https://openai.com/ja-JP/news/rss.xml",
			"https://openai.com/index/article", "https://openai.com/ja-JP/index/article",
			"https://medium.com/feed/test", "https://publication.medium.com/article",
			"https://example.com/feed.xml", "https://notopenai.com/article", "https://openai.com.example.com/article",
		} {
			t.Run(custom+raw, func(t *testing.T) {
				t.Setenv("ARTICLE_USER_AGENT", custom)
				isTarget := strings.Contains(raw, "https://openai.com/") || strings.Contains(raw, "medium.com/")
				for _, article := range []bool{false, true} {
					calls := 0
					client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						want := ""
						if article {
							want = defaultArticleUserAgent
							if strings.TrimSpace(custom) != "" {
								want = strings.TrimSpace(custom)
							}
						}
						if isTarget {
							want = browserSourceUserAgent
							if strings.TrimSpace(custom) != "" {
								want = strings.TrimSpace(custom)
							}
						}
						if got := req.Header.Get("User-Agent"); got != want {
							t.Errorf("UA article=%v got %q want %q", article, got, want)
						}
						body := sourceTestRSS
						if article {
							body = fixtureHTML("Article", "")
						}
						return fetchResponse(req, 200, body, nil), nil
					})}
					if article {
						if _, err := fetchArticle(context.Background(), client, raw, "", ""); err != nil {
							t.Fatal(err)
						}
						if calls != 2 {
							t.Fatal("HEAD/GETの両方を検証していません")
						}
					} else {
						if f, err := fetchFeed(context.Background(), client, raw); err != nil || len(f.Items) != 1 {
							t.Fatal("RSSの解析に失敗", err)
						}
					}
				}
			})
		}
	}
}

func TestSourceRedirectPolicy(t *testing.T) {
	t.Setenv("ARTICLE_USER_AGENT", "")
	for _, status := range []int{301, 302, 308} {
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Host {
			case "example.com":
				if req.Header.Get("User-Agent") != "" {
					t.Error("他ドメインのRSSにUAを変更しました")
				}
				return fetchResponse(req, status, "", map[string]string{"Location": "https://openai.com/news/rss.xml"}), nil
			case "openai.com":
				if req.Header.Get("User-Agent") != browserSourceUserAgent {
					t.Error("転送先OpenAIのUAが不正")
				}
				return fetchResponse(req, status, "", map[string]string{"Location": "https://other.example/feed.xml"}), nil
			default:
				if req.Header.Get("User-Agent") != "" {
					t.Error("専用UAを他ドメインへ引き継ぎました")
				}
				return fetchResponse(req, 200, sourceTestRSS, nil), nil
			}
		})}
		if _, err := fetchFeed(context.Background(), client, "https://example.com/feed.xml"); err != nil {
			t.Fatal(err)
		}
		// 呼び出し元のリダイレクト禁止設定を維持する。
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if _, err := fetchFeed(context.Background(), client, "https://example.com/feed.xml"); err == nil {
			t.Fatal("既存CheckRedirectを上書きしました")
		}
	}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return fetchResponse(req, 302, "", map[string]string{"Location": req.URL.String()}), nil
	})}
	if _, err := fetchFeed(context.Background(), client, openAIFeedURL); err == nil || requests != 10 {
		t.Fatal("リダイレクトを無制限に繰り返しました", requests, err)
	}
}

func TestOpenAI403FallbackAndFeedFailure(t *testing.T) {
	t.Setenv("ARTICLE_USER_AGENT", "")
	t.Setenv("CRAWLBASE_TOKEN", "test-token")
	r := testRunner(t, nil)
	proxyCalls, sourceCalls := 0, 0
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.crawlbase.com" {
			proxyCalls++
			if req.URL.Query().Get("url") != openAITestEnglish || req.URL.Query().Get("user_agent") != "" || req.Header.Get("User-Agent") == browserSourceUserAgent {
				t.Fatal("CrawlbaseのUA方針を変更しました")
			}
			return fetchResponse(req, 200, fixtureHTML("Article", ""), map[string]string{"cb_status": "200", "original_status": "200", "url": openAITestEnglish}), nil
		}
		sourceCalls++
		if req.Header.Get("User-Agent") != browserSourceUserAgent {
			t.Fatal("OpenAIのUAが不正")
		}
		return fetchResponse(req, 403, "", nil), nil
	})}
	var err error
	r.crawlbase, err = newCrawlbaseClient(r.client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetchArticle(context.Background(), openAITestEnglish, "Article", ""); err == nil {
		t.Fatal("403を成功扱いしました")
	}
	if proxyCalls != 0 || r.state.Fetch[hash(openAITestEnglish)].Method != fetchCrawlbase {
		t.Fatal("次回へのフォールバックが不正")
	}
	if _, err := r.fetchArticle(context.Background(), openAITestEnglish, "Article", ""); !errors.Is(err, errFetchDeferred) {
		t.Fatal("同一実行内で再試行しました", err)
	}
	r.fetchAttempted = nil
	if a, err := r.fetchArticle(context.Background(), openAITestEnglish, "Article", ""); err != nil || !strings.Contains(a.Markdown, "12ms") {
		t.Fatal("Crawlbase取得に失敗", err)
	}
	if proxyCalls != 1 || sourceCalls != 2 {
		t.Fatal("不要な再試行", proxyCalls, sourceCalls)
	}
	// RSSには従来どおりCrawlbase経路がなく、1回で呼び出し元へ失敗を返す。
	before := sourceCalls
	if _, err := fetchFeed(context.Background(), r.client, openAIFeedURL); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatal("RSSの403処理が不正", err)
	}
	if sourceCalls != before+1 || proxyCalls != 1 {
		t.Fatal("RSSで不要な再試行・代替取得をしました")
	}
}
