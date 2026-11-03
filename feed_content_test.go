package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestFeedContentProcessing(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		enabled       bool
		calls         int
	}{
		{"RSS本文", fixtureHTML("技術記事", ""), true, 0},
		{"本文なし", "", true, 2},
		{"空白", "  ", true, 2},
		{"無効", fixtureHTML("技術記事", ""), false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai, _, summaries := testAI(t)
			r := testRunner(t, ai)
			calls := 0
			r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return fetchResponse(req, 200, fixtureHTML("技術記事", ""), nil), nil
			})}
			f := Feed{Name: "Netflix", URL: "https://netflixtechblog.com/feed", Mode: "all", FeedContent: tc.enabled}
			item := &gofeed.Item{Title: "技術記事", Link: "https://netflixtechblog.com/article", Content: tc.content, Description: "抜粋"}
			posts := 0
			r.post = func(_, _ string) error { posts++; return nil }
			for range 2 {
				if err := r.processFeedItem(context.Background(), f, item, item.Link, "", itemKeys(f, item)); err != nil {
					t.Fatal(err)
				}
			}
			if calls != tc.calls || posts != 1 || *summaries != 1 || !r.state.done(itemKeys(f, item)) {
				t.Fatalf("calls=%d posts=%d summaries=%d", calls, posts, *summaries)
			}
		})
	}
}

func TestNetflixFeedContentConfig(t *testing.T) {
	feeds, err := readFeeds("feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.URL == "https://netflixtechblog.com/feed" {
			if !f.FeedContent {
				t.Fatal("Netflix RSS本文の利用が無効です")
			}
			return
		}
	}
	t.Fatal("Netflixのフィードがありません")
}
