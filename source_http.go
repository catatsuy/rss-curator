package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

const browserSourceUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

func (r *runner) initialFetchMethod() string {
	if r.skipHTTP {
		return fetchJA4
	}
	return fetchHTTP
}

func (r *runner) browserClient() (*http.Client, error) {
	if r.workaroundClient == nil {
		client, err := newHTTPWorkaroundClient(r.client, "")
		if err != nil {
			return nil, err
		}
		r.workaroundClient = client
	}
	return r.workaroundClient, nil
}

// RSSも記事と同じ順番で取得する。dry-runでは有料APIを利用しない。
func (r *runner) fetchFeed(ctx context.Context, raw string) (*gofeed.Feed, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if r.fetchTime().Before(r.state.HostRetryAfter[hash(parsed.Hostname())]) {
		return nil, errFetchDeferred
	}
	var feed *gofeed.Feed
	for _, method := range []string{fetchHTTP, fetchJA4, fetchCrawlbase} {
		if method == fetchHTTP && r.initialFetchMethod() == fetchJA4 {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if method == fetchCrawlbase {
			if r.dry || r.crawlbaseUnavailable(r.fetchTime()) != "" {
				return nil, err
			}
			resp, result, fetchErr := r.crawlbase.fetchResponse(ctx, raw, map[string]bool{})
			if fetchErr != nil {
				if result.ServiceLimited {
					r.state.CrawlbaseRetryAfter = retryAfter(result.RetryAfter, r.fetchTime())
				}
				if result.RateLimited {
					if r.state.HostRetryAfter == nil {
						r.state.HostRetryAfter = map[string]time.Time{}
					}
					r.state.HostRetryAfter[hash(parsed.Hostname())] = retryAfter(result.RetryAfter, r.fetchTime())
				}
				return nil, fetchErr
			}
			feed, err = readFeedResponse(resp)
		} else {
			client := r.client
			if method == fetchJA4 {
				client, err = r.browserClient()
				if err != nil {
					continue
				}
			}
			feed, err = fetchFeed(ctx, client, raw)
		}
		if err == nil {
			return feed, nil
		}
		var status *articleHTTPError
		if errors.As(err, &status) {
			if status.Status == 429 {
				if r.state.HostRetryAfter == nil {
					r.state.HostRetryAfter = map[string]time.Time{}
				}
				u, _ := url.Parse(raw)
				until := retryAfter(status.RetryAfter, r.fetchTime())
				r.state.HostRetryAfter[hash(u.Hostname())] = until
				r.state.HostRetryAfter[hash(status.Host)] = until
				return nil, err
			}
			switch status.Status {
			case 401, 404, 410, 451:
				return nil, err
			}
		}
		slog.Info("RSS取得の失敗", "url", safeArticleURL(raw), "method", method)
	}
	return nil, err
}

// cf-mitigatedはCloudflareが公開しているChallenge Pageの識別ヘッダー。
// HTMLの推測は、確認画面のタイトルと実際の確認用要素が揃った場合に限る。
func isBotChallenge(resp *http.Response, doc *html.Node) bool {
	if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") || strings.EqualFold(resp.Header.Get("original_header_cf-mitigated"), "challenge") {
		return true
	}
	title, marker := "", false
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "title" {
			title = strings.ToLower(strings.TrimSpace(textContent(n)))
		}
		if n.Data == "script" && strings.Contains(attr(n, "src"), "/cdn-cgi/challenge-platform/") {
			marker = true
		}
		if attr(n, "id") == "challenge-form" || attr(n, "id") == "challenge-running" {
			marker = true
		}
	})
	return marker && (strings.HasPrefix(title, "just a moment") || title == "verify you are human" || title == "checking your browser")
}
