package main

import (
	"path/filepath"
	"testing"
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
