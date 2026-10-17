package main

import "testing"

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
