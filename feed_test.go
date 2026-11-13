package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestOpenAIJapaneseOption(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		wantError    bool
	}{
		{"enabled", `[{"name":"OpenAI","url":"https://openai.com/news/rss.xml","mode":"select","openai_japanese":true}]`, false},
		{"disabled", `[{"name":"OpenAI","url":"https://openai.com/news/rss.xml","mode":"select","openai_japanese":false}]`, false},
		{"other-feed", `[{"name":"Other","url":"https://example.com/rss","mode":"select","openai_japanese":true}]`, true},
		{"other-mode", `[{"name":"OpenAI","url":"https://openai.com/news/rss.xml","mode":"all","openai_japanese":true}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "feeds.json")
			mustWrite(t, path, tc.config)
			feeds, err := readFeeds(path)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected config validation: %v", err)
			}
			if err == nil && feeds[0].OpenAIJapanese != (tc.name == "enabled") {
				t.Fatal("オプションが読み込まれていません")
			}
		})
	}
}

func TestFeedPathSuffix(t *testing.T) {
	feeds, err := readFeeds("feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	var gfw Feed
	for _, f := range feeds {
		if f.Name == "GFW Report" {
			gfw = f
		}
	}
	if gfw.PathSuffix != "/en/" {
		t.Fatal("GFW Reportの英語版フィルターがありません")
	}
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://gfw.report/publications/pets26b/en/", true},
		{"https://gfw.report/publications/pets26b/en/?source=rss#abstract", true},
		{"https://gfw.report/publications/pets26b/zh/", false},
		{"https://gfw.report/publications/pets26b/zh/?lang=/en/", false},
	} {
		if got := gfw.includes(tc.url); got != tc.want {
			t.Errorf("includes(%q) = %v, want %v", tc.url, got, tc.want)
		}
		if !(Feed{}).includes(tc.url) {
			t.Error("フィルター未指定のフィードが除外されました")
		}
	}
}

func TestSkipHTTPConfig(t *testing.T) {
	for _, option := range []string{"", `,"skip_http":false`, `,"skip_http":true`} {
		path := filepath.Join(t.TempDir(), "feeds.json")
		mustWrite(t, path, `[{"name":"Source","url":"https://example.com/feed","mode":"select"`+option+`}]`)
		feeds, err := readFeeds(path)
		if err != nil || feeds[0].SkipHTTP != (option == `,"skip_http":true`) {
			t.Fatal(feeds, err)
		}
	}
	feeds, err := readFeeds("feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, feed := range feeds {
		if feed.SkipHTTP != (feed.Name == "OpenAI") {
			t.Fatal("通常HTTP省略の既定設定が不正です", feed)
		}
	}
}

func TestResolveFeedLinks(t *testing.T) {
	base := "https://gvisor.dev/blog/index.xml"
	for _, tc := range []struct {
		raw, want string
	}{
		{"/blog/2026/11/12/checkpoint-restore", "https://gvisor.dev/blog/2026/11/12/checkpoint-restore"},
		{"2026/11/12/checkpoint-restore", "https://gvisor.dev/blog/2026/11/12/checkpoint-restore"},
		{"//example.com/article", "https://example.com/article"},
		{"https://example.com/article?source=rss", "https://example.com/article?source=rss"},
		{"javascript:alert(1)", "javascript:alert(1)"},
		{"http://%", "http://%"},
		{"", ""},
	} {
		if got := resolveFeedLink(base, tc.raw); got != tc.want {
			t.Errorf("resolveFeedLink(%q, %q) = %q, want %q", base, tc.raw, got, tc.want)
		}
	}
}

func TestResolveFeedItemLinks(t *testing.T) {
	feed := Feed{Name: "gVisor Blog", URL: "https://gvisor.dev/blog/index.xml"}
	item := &gofeed.Item{
		Link: "/blog/2026/11/12/checkpoint-restore",
		GUID: "/blog/2026/11/12/checkpoint-restore",
		Enclosures: []*gofeed.Enclosure{
			{URL: "/papers/checkpoint.pdf", Type: "application/pdf"},
		},
	}
	previousKeys := itemKeys(feed, item)
	if len(previousKeys) != 1 {
		t.Fatal("相対URLに対してURLハッシュが生成されました", previousKeys)
	}
	resolveFeedItemLinks(feed, item)
	const want = "https://gvisor.dev/blog/2026/11/12/checkpoint-restore"
	if item.Link != want || item.Enclosures[0].URL != "https://gvisor.dev/papers/checkpoint.pdf" {
		t.Fatal("RSS内の相対URLが解決されませんでした", item)
	}
	keys := itemKeys(feed, item)
	if len(keys) != 2 || keys[0] != hash(want) || keys[1] != previousKeys[0] {
		t.Fatal("元のGUIDキーとの互換性が失われました", keys, previousKeys)
	}
	resolveFeedItemLinks(feed, item)
	if item.Link != want || itemKeys(feed, item)[1] != previousKeys[0] {
		t.Fatal("相対URLの解決が冪等ではありません")
	}

	withoutLink := &gofeed.Item{GUID: "/blog/2026/10/02/gvisor-cncf"}
	resolveFeedItemLinks(feed, withoutLink)
	if withoutLink.Link != "https://gvisor.dev/blog/2026/10/02/gvisor-cncf" {
		t.Fatal("URL形式のGUIDから記事リンクを補完できませんでした", withoutLink.Link)
	}
	opaqueGUID := &gofeed.Item{GUID: "opaque-entry-id"}
	resolveFeedItemLinks(feed, opaqueGUID)
	if opaqueGUID.Link != "" {
		t.Fatal("識別子だけのGUIDを記事URLとして扱いました", opaqueGUID.Link)
	}
}

func TestRunRelativeRSSLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss version="2.0"><channel><title>Test</title><link>https://gvisor.dev/</link><description>Test</description>
			<item><title>Relative link</title><link>/blog/2026/11/12/checkpoint-restore</link><guid>/blog/2026/11/12/checkpoint-restore</guid></item>
			<item><title>GUID only</title><guid>/blog/2026/10/02/gvisor-cncf</guid></item>
			</channel></rss>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "feeds.json")
	mustWrite(t, config, fmt.Sprintf(`[{"name":"gVisor Blog","url":%q,"mode":"all"}]`, server.URL+"/blog/index.xml"))
	output := filepath.Join(dir, "out")
	if err := run(context.Background(), config, filepath.Join(dir, "state.json"), output, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Articles) != 2 {
		t.Fatal("RSSの2件を処理できませんでした", manifest.Articles)
	}
	expected := []string{server.URL + "/blog/2026/11/12/checkpoint-restore", server.URL + "/blog/2026/10/02/gvisor-cncf"}
	for i, entry := range manifest.Articles {
		if entry.URL != expected[i] {
			t.Errorf("article %d: URL = %q, want %q", i, entry.URL, expected[i])
		}
	}
}
