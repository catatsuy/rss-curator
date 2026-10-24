package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

const openAIFeedURL = "https://openai.com/news/rss.xml"

func openAIEnglishURL(raw string) string {
	normalized, err := normalize(raw)
	if err != nil {
		return ""
	}
	u, _ := url.Parse(normalized)
	if u.Host != "openai.com" || u.Scheme != "https" {
		return ""
	}
	if strings.HasPrefix(u.Path, "/ja-JP/") {
		u.Path = strings.TrimPrefix(u.Path, "/ja-JP")
	}
	u.RawPath = ""
	if u.Path == "" || u.Path == "/" {
		return ""
	}
	return u.String()
}

func openAIJapaneseURL(english string) string {
	u, _ := url.Parse(english)
	u.Path = "/ja-JP" + u.Path
	u.RawPath = ""
	return u.String()
}

// 言語メタデータやナビゲーションだけではなく、抽出した本文で確認する。
func hasJapaneseArticle(a Article, target string) bool {
	final, err := url.Parse(a.finalURL)
	want, _ := url.Parse(target)
	if err != nil || final.Host != want.Host || strings.TrimRight(final.Path, "/") != strings.TrimRight(want.Path, "/") || a.PDF || a.Paper {
		return false
	}
	doc, err := html.Parse(strings.NewReader(a.HTML))
	if err != nil {
		return false
	}
	kana, japanese, letters := 0, 0, 0
	for _, ch := range textContent(doc) {
		if unicode.IsLetter(ch) {
			letters++
		}
		if unicode.In(ch, unicode.Hiragana, unicode.Katakana) {
			kana++
		}
		if unicode.In(ch, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			japanese++
		}
	}
	return kana >= 20 && japanese >= 80 && japanese*4 >= letters
}

func (r *runner) processFeedItem(ctx context.Context, f Feed, item *gofeed.Item, raw, mime string, keys []string) error {
	english := openAIEnglishURL(raw)
	if !f.OpenAIJapanese || f.URL != openAIFeedURL || f.Mode != "select" || english == "" || mime != "" || r.dry {
		return r.process(ctx, f.Name, f.Mode, raw, item.Title, mime, "", keys)
	}
	japanese := openAIJapaneseURL(english)
	id := hash(english)
	keys = append(keys, id)
	if r.state.done(keys) || r.claimed[id] {
		return nil
	}
	probe := r.state.Fetch[hash(japanese)]
	if probe.Stopped {
		return r.process(ctx, f.Name, "select", english, item.Title, "", "", keys)
	}
	// 取得できないことが既知の間は、本文取得も興味判定も行わない。
	now := r.fetchTime()
	if now.Before(probe.NextAttempt) || now.Before(r.state.HostRetryAfter[hash("openai.com")]) {
		return nil
	}
	if probe.Method == fetchCrawlbase && (r.crawlbase == nil || r.crawlbase.token == "" || r.crawlbase.disabled || r.crawlbase.used >= r.crawlbase.limit || now.Before(r.state.CrawlbaseRetryAfter)) {
		return nil
	}
	a, err := r.fetchArticleWithPolicy(ctx, japanese, item.Title, "", true)
	if errors.Is(err, errDelivery) {
		return err
	}
	fetchState := r.state.Fetch[hash(japanese)]
	if err != nil && !fetchState.Stopped {
		// 403/429、通信失敗、待機・利用上限は次回に再試行する。
		if errors.Is(err, errFetchDeferred) {
			return nil
		}
		return err
	}
	if err != nil || !hasJapaneseArticle(a, japanese) {
		return r.process(ctx, "OpenAI", "select", english, item.Title, "", "", keys)
	}
	// 日本語ページが英語版canonicalを宣言していても、日本語の最終URLを投稿する。
	canonicalEnglish := openAIEnglishURL(a.URL)
	if canonicalEnglish != "" {
		id = hash(canonicalEnglish)
	}
	keys = append(keys, hash(japanese), hash(a.finalURL), hash(a.URL), id)
	already := r.state.done(keys) || r.claimed[id]
	// 既存の処理済みキーを別の未処理IDへ付け替えない。
	for _, key := range keys {
		if _, done := r.state.Processed[key]; done {
			id = key
			break
		}
		if existing := r.state.Aliases[key]; existing != "" {
			if _, done := r.state.Processed[existing]; done {
				id = existing
				break
			}
		}
	}
	for _, key := range keys {
		r.state.Aliases[key] = id
	}
	if already {
		return nil
	}
	// 日本語本文がある場合だけ、RSS情報で興味判定する。
	metadata := item.Description
	if item.Content != "" {
		metadata += "\n" + item.Content
	}
	doc, _ := html.Parse(strings.NewReader(metadata))
	accepted, err := r.ai.interest(ctx, Article{Title: item.Title, URL: english, Markdown: "RSSのタイトルと概要（記事本文ではありません）:\n" + item.Title + "\n" + textContent(doc)}, "")
	if err != nil {
		return err
	}
	if !accepted {
		r.state.Processed[id] = r.fetchTime()
		return nil
	}
	e := Entry{ID: id, Title: strings.Join(strings.Fields(item.Title), " "), URL: a.finalURL, Post: true}
	r.manifest.Articles = append(r.manifest.Articles, e)
	if err := r.deliver(e, "link\n"+e.URL+"\n"); err != nil {
		return err
	}
	r.claimed[id] = true
	return nil
}
