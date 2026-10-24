package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

type Feed struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Mode           string `json:"mode"`
	PathSuffix     string `json:"path_suffix,omitempty"`
	OpenAIJapanese bool   `json:"openai_japanese,omitempty"`
}

func (f Feed) includes(raw string) bool {
	if f.PathSuffix == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(u.Path, f.PathSuffix)
}

type Entry struct {
	ID    string `json:"id"`
	Path  string `json:"path,omitempty"`
	Post  bool   `json:"post"`
	Title string `json:"title"`
	URL   string `json:"url"`
}
type Manifest struct {
	DryRun   bool    `json:"dry_run"`
	Articles []Entry `json:"articles"`
}
type runner struct {
	client            *http.Client
	ai                *AI
	state             *State
	statePath, output string
	dry               bool
	manifest          Manifest
	claimed           map[string]bool
	post              func(string, string) error
	checkpoint        func() error
	crawlbase         *crawlbaseClient
	fetchAttempted    map[string]bool
	now               func() time.Time
	maxFetchFailures  int
}

var errDelivery = errors.New("投稿処理を継続できません")

func readFeeds(path string) ([]Feed, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, fmt.Errorf("フィード設定を読めません: %w", e)
	}
	var feeds []Feed
	if e = json.Unmarshal(b, &feeds); e != nil {
		return nil, fmt.Errorf("フィード設定のJSONが不正です: %w", e)
	}
	if len(feeds) == 0 {
		return nil, fmt.Errorf("フィード設定が空です")
	}
	seen := map[string]bool{}
	for i := range feeds {
		f := &feeds[i]
		u, e := normalize(f.URL)
		if e != nil || f.Name == "" || seen[u] {
			return nil, fmt.Errorf("フィード名・URLが不正、または重複しています")
		}
		f.URL = u
		seen[u] = true
		if f.OpenAIJapanese && (f.URL != openAIFeedURL || f.Mode != "select") {
			return nil, fmt.Errorf("openai_japaneseはOpenAI公式RSSのselectモードでのみ指定できます")
		}
		if f.Mode != "select" && f.Mode != "all" && f.Mode != "digest" {
			return nil, fmt.Errorf("フィードのモードが不正です")
		}
	}
	return feeds, nil
}
func fetchFeed(ctx context.Context, c *http.Client, u string) (*gofeed.Feed, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", u, nil)
	if e != nil {
		return nil, e
	}
	resp, e := c.Do(req)
	if e != nil {
		return nil, fmt.Errorf("RSSを取得できません")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("RSSのHTTPステータス: %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, (10<<20)+1))
	if e != nil || len(b) > 10<<20 {
		return nil, fmt.Errorf("RSSの読み込みに失敗、または10MiBを超えています")
	}
	f, e := gofeed.NewParser().ParseString(string(b))
	if e != nil {
		return nil, fmt.Errorf("RSS / Atomを解析できません")
	}
	return f, nil
}
func itemDate(i *gofeed.Item) time.Time {
	if i.PublishedParsed != nil {
		return *i.PublishedParsed
	}
	if i.UpdatedParsed != nil {
		return *i.UpdatedParsed
	}
	return time.Time{}
}
func itemKeys(f Feed, i *gofeed.Item) []string {
	keys := []string{}
	if u, e := normalize(i.Link); e == nil {
		keys = append(keys, hash(u))
	}
	if i.GUID != "" {
		keys = append(keys, hash(f.URL+"\n"+i.GUID))
	}
	return keys
}
func (s *State) done(keys []string) bool {
	for _, k := range keys {
		if _, ok := s.Processed[k]; ok {
			return true
		}
		if id := s.Aliases[k]; id != "" {
			if _, ok := s.Processed[id]; ok {
				return true
			}
		}
	}
	return false
}
func (r *runner) save() error {
	if r.dry {
		return nil
	}
	r.state.pruneFetch(r.fetchTime())
	if e := writeJSON(r.statePath, r.state); e != nil {
		return fmt.Errorf("状態の保存に失敗しました: %w", e)
	}
	return nil
}
func (r *runner) emit(a Article, source, id, summary string) error {
	if strings.TrimSpace(a.Title) == "" {
		a.Title = a.URL
	}
	e := Entry{ID: id, Title: a.Title, URL: a.URL, Post: !r.dry}
	if !r.dry {
		e.Path = filepath.Join("pending", id+".md")
		body := "# " + strings.Join(strings.Fields(a.Title), " ") + "\n\n原文URL: " + a.URL + "\n"
		if !a.PDF && !a.Paper {
			body += "\n情報源: " + source + "\n\n" + summary + "\n"
		}
		if err := atomicWrite(filepath.Join(r.output, e.Path), []byte(body)); err != nil {
			return err
		}
	}
	r.manifest.Articles = append(r.manifest.Articles, e)
	return r.deliver(e, filepath.Join(r.output, e.Path))
}

func (r *runner) deliver(e Entry, payload string) error {
	id := e.ID
	if !r.dry && r.post != nil {
		if err := r.post(id, payload); err != nil {
			return fmt.Errorf("%w: %v", errDelivery, err)
		}
		r.state.Processed[id] = time.Now().UTC()
		if err := r.save(); err != nil {
			return fmt.Errorf("%w: %v", errDelivery, err)
		}
		if r.checkpoint != nil {
			if err := r.checkpoint(); err != nil {
				return fmt.Errorf("%w: 状態の永続化に失敗しました", errDelivery)
			}
		}
	}
	return nil
}
func (r *runner) process(ctx context.Context, source, mode, raw, title, mime, intro string, keys []string) error {
	if r.dry {
		u, e := normalize(raw)
		if e != nil {
			return e
		}
		id := hash(u)
		if r.claimed[id] || r.state.done(keys) {
			return nil
		}
		r.claimed[id] = true
		return r.emit(Article{Title: title, URL: u}, source, id, "")
	}
	if u, err := normalize(raw); err == nil && r.state.Fetch[hash(u)].FailureNoticePending {
		return r.notifyFetchFailure(u)
	}
	// digestは紹介先の再試行が必要な場合だけ、投稿済み本体を再取得する。
	if mode != "digest" && r.state.done(keys) {
		return nil
	}
	marker := ""
	if mode == "digest" {
		marker = hash("digest\n" + keys[0])
		if r.state.done(keys) && r.state.Aliases[marker] == "" {
			return nil
		}
		if !r.digestNeedsFetch(marker, keys) {
			return nil
		}
		r.state.Aliases[marker] = keys[0]
	}
	a, e := r.fetchArticle(ctx, raw, title, mime)
	if errors.Is(e, errFetchDeferred) {
		return nil
	}
	if e != nil {
		return e
	}
	id := hash(a.URL)
	keys = append(keys, id)
	for _, k := range keys {
		r.state.Aliases[k] = id
	}
	already := r.state.done(keys) || r.claimed[id]
	var links []Link
	if mode == "digest" {
		if !a.PDF && !a.Paper {
			links = digestLinks(a)
		}
		r.rememberDigest(marker, links)
		// 投稿成功時のcheckpointより前に紹介先一覧を保存する。
		if err := r.save(); err != nil {
			return fmt.Errorf("%w: %v", errDelivery, err)
		}
	}
	articleErr := func() error {
		if already {
			return nil
		}
		accepted := true
		if mode == "select" && !a.PDF && !a.Paper {
			accepted, e = r.ai.interest(ctx, a, intro)
			if e != nil {
				return e
			}
		}
		if !accepted {
			r.state.Processed[id] = time.Now().UTC()
		} else {
			summary := ""
			if !a.PDF && !a.Paper {
				summary, e = r.ai.summarize(ctx, a)
				if e != nil {
					return e
				}
			}
			if e = r.emit(a, source, id, summary); e != nil {
				return e
			}
			r.claimed[id] = true
		}
		return nil
	}()
	if errors.Is(articleErr, errDelivery) {
		return articleErr
	}
	if mode == "digest" {
		for _, link := range links {
			if e = r.process(ctx, source+"の紹介", "select", link.URL, link.Title, "", link.Context, []string{hash(link.URL)}); e != nil {
				if errors.Is(e, errDelivery) {
					return e
				}
				slog.Error("紹介先記事の処理に失敗しました", "id", hash(link.URL), "エラー", e)
			}
			if err := r.save(); err != nil {
				return err
			}
		}
		r.digestNeedsFetch(marker, keys)
	}
	return articleErr
}
func run(ctx context.Context, config, statePath, output string, dry bool) error {
	return runWithDelivery(ctx, config, statePath, output, dry, nil, nil)
}

func runWithDelivery(ctx context.Context, config, statePath, output string, dry bool, post func(string, string) error, checkpoint func() error) error {
	feeds, e := readFeeds(config)
	if e != nil {
		return e
	}
	s, e := loadState(statePath)
	if e != nil {
		return e
	}
	var ai *AI
	if !dry {
		ai, e = newAI()
		if e != nil {
			return e
		}
	}
	// 古い投稿用ファイルが混入しないよう出力先の再利用を禁止する。
	if _, e = os.Stat(output); e == nil {
		return fmt.Errorf("出力先は存在しないディレクトリを指定してください")
	}
	if !os.IsNotExist(e) {
		return e
	}
	if e = os.MkdirAll(filepath.Join(output, "pending"), 0700); e != nil {
		return e
	}
	r := &runner{client: &http.Client{Timeout: 90 * time.Second}, ai: ai, state: s, statePath: statePath, output: output, dry: dry, manifest: Manifest{DryRun: dry, Articles: []Entry{}}, claimed: map[string]bool{}}
	if !dry {
		r.maxFetchFailures, e = fetchFailureLimit()
		if e != nil {
			return e
		}
		r.crawlbase, e = newCrawlbaseClient(r.client)
		if e != nil {
			return e
		}
		s.pruneFetch(r.fetchTime())
		r.post = post
		r.checkpoint = checkpoint
	}
	for _, f := range feeds {
		feed, e := fetchFeed(ctx, r.client, f.URL)
		if e != nil {
			slog.Error("RSSの処理に失敗しました", "情報源", f.Name, "エラー", e)
			continue
		}
		sort.SliceStable(feed.Items, func(i, j int) bool { return itemDate(feed.Items[i]).After(itemDate(feed.Items[j])) })
		first := !s.Initialized[hash(f.URL)]
		unique := map[string]bool{}
		count := 0
		var targets []*gofeed.Item
		for _, item := range feed.Items {
			if !f.includes(item.Link) {
				continue
			}
			keys := itemKeys(f, item)
			if len(keys) == 0 {
				slog.Error("記事のURLとGUIDがありません", "情報源", f.Name)
				continue
			}
			duplicate := false
			for _, k := range keys {
				if unique[k] {
					duplicate = true
				}
				unique[k] = true
			}
			if duplicate {
				continue
			}
			count++
			if first && count > 2 {
				for _, k := range keys {
					s.Processed[k] = time.Now().UTC()
				}
				continue
			}
			targets = append(targets, item)
		}
		s.Initialized[hash(f.URL)] = true
		if e = r.save(); e != nil {
			return e
		}
		for _, item := range targets {
			keys := itemKeys(f, item)
			mime := ""
			raw := item.Link
			for _, enc := range item.Enclosures {
				if isPDF(enc.URL, enc.Type) {
					raw = enc.URL
					mime = enc.Type
					break
				}
			}
			if e = r.processFeedItem(ctx, f, item, raw, mime, keys); e != nil {
				if errors.Is(e, errDelivery) {
					return e
				}
				slog.Error("記事の処理に失敗しました", "id", keys[0], "エラー", e)
			}
			if e = r.save(); e != nil {
				return e
			}
			if e = writeJSON(filepath.Join(output, "manifest.json"), r.manifest); e != nil {
				return e
			}
		}
	}
	if e = r.save(); e != nil {
		return e
	}
	if e = writeJSON(filepath.Join(output, "manifest.json"), r.manifest); e != nil {
		return e
	}
	// ShellはJSONを解釈せず、このIDリストだけを読む。パスはIDから決定する。
	var ids strings.Builder
	for _, entry := range r.manifest.Articles {
		if entry.Post {
			fmt.Fprintln(&ids, entry.ID)
		}
	}
	if e = atomicWrite(filepath.Join(output, "upload-ids"), []byte(ids.String())); e != nil {
		return e
	}
	if dry {
		b, _ := json.MarshalIndent(r.manifest, "", "  ")
		fmt.Println(string(b))
	}
	return nil
}
