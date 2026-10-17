package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func fixtureHTML(title, extra string) string {
	b, err := os.ReadFile("testdata/article.html")
	if err != nil {
		panic(err)
	}
	return strings.NewReplacer("{{TITLE}}", title, "{{EXTRA}}", extra).Replace(string(b))
}
func TestNormalize(t *testing.T) {
	got, e := normalize("https://EXAMPLE.com:443/a?id=12&utm_source=x&fbclid=abc#part")
	if e != nil || got != "https://example.com/a?id=12" {
		t.Fatalf("正規化: %s %v", got, e)
	}
	for _, u := range []string{"javascript:alert(1)", "file:///tmp/x", "https://u:p@example.com/a", "/relative"} {
		if _, e := normalize(u); e == nil {
			t.Fatalf("不正URLを受理: %s", u)
		}
	}
}
func TestCanonicalAndExtraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/alias", http.StatusFound)
			return
		}
		fmt.Fprint(w, strings.Replace(fixtureHTML("実装の詳細", ""), "</head>", `<link rel="canonical" href="/canonical?utm_source=x"></head>`, 1))
	}))
	defer server.Close()
	a, e := fetchArticle(context.Background(), server.Client(), server.URL+"/redirect", "元タイトル", "")
	if e != nil {
		t.Fatal(e)
	}
	if a.URL != server.URL+"/canonical" {
		t.Fatal(a.URL)
	}
	for _, s := range []string{"func Serve()", "64", "12ms", "Connection flow", server.URL + "/reference"} {
		if !strings.Contains(a.Markdown, s) {
			t.Errorf("Markdownに必要な要素がありません: %s\n%s", s, a.Markdown)
		}
	}
	for _, can := range []string{"https://evil.example/other", "javascript:alert(1)", "/"} {
		doc, _ := parseHTMLForTest(`<link rel="canonical" href="` + can + `">`)
		if canonical(doc, server.URL+"/article") != server.URL+"/article" {
			t.Errorf("不適切なcanonicalを採用: %s", can)
		}
	}
}
func TestPDFNeverRead(t *testing.T) {
	for _, method := range []string{"HEAD", "GET"} {
		t.Run(method, func(t *testing.T) {
			read := 0
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := http.Header{}
				status := 200
				if method == "GET" && r.Method == "HEAD" {
					status = 405
				} else {
					h.Set("Content-Type", "application/pdf")
				}
				return &http.Response{StatusCode: status, Header: h, Body: &countBody{reads: &read}, Request: r}, nil
			})}
			a, e := fetchArticle(context.Background(), c, "https://example.com/download", "PDF", "")
			if e != nil || !a.PDF || read != 0 {
				t.Fatalf("PDF本文を読みました: %+v %v %d", a, e, read)
			}
		})
	}
	calls := 0
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("呼び出されてはいけません")
	})}
	a, e := fetchArticle(context.Background(), c, "https://example.com/paper.pdf", "論文", "")
	if e != nil || !a.PDF || calls != 0 {
		t.Fatal(a, e, calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countBody struct{ reads *int }

func (b *countBody) Read(p []byte) (int, error) { *b.reads++; return 0, io.EOF }
func (*countBody) Close() error                 { return nil }
func TestScoreDecision(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		want, fail bool
	}{{`{"answers":[{"type":"score","name":"interest","score":0.9}]}`, true, false}, {`{"answers":[{"type":"score","name":"interest","score":0.2}]}`, false, false}, {`{"answers":[{"type":"refusal","name":"interest"}]}`, false, true}, {`{"answers":[{"type":"score","name":"interest"}]}`, false, true}, {`{"answers":[]}`, false, true}} {
		var d openai.Decision
		if e := json.Unmarshal([]byte(tc.raw), &d); e != nil {
			t.Fatal(e)
		}
		got, e := scoreDecision(&d, 0.8)
		if got != tc.want || (e != nil) != tc.fail {
			t.Fatalf("判定結果: %v %v", got, e)
		}
	}
}
func TestFeedParsingAndDiscovery(t *testing.T) {
	for _, atom := range []bool{false, true} {
		t.Run(fmt.Sprint(atom), func(t *testing.T) {
			n := 4
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atom {
					fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>テスト</title><id>feed</id>`)
				} else {
					fmt.Fprint(w, `<rss version="2.0"><channel><title>テスト</title><link>https://example.com</link><description>テスト</description>`)
				}
				for i := 1; i <= n; i++ {
					date := time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC)
					if i == 5 {
						date = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
					}
					if atom {
						fmt.Fprintf(w, `<entry><id>%d</id><title>記事%d</title><link href="%s/%d"/><published>%s</published></entry>`, i, i, base, i, date.Format(time.RFC3339))
					} else {
						fmt.Fprintf(w, `<item><guid>%d</guid><title>記事%d</title><link>%s/%d</link><pubDate>%s</pubDate></item>`, i, i, base, i, date.Format(time.RFC1123Z))
					}
				}
				if atom {
					fmt.Fprint(w, `</feed>`)
				} else {
					fmt.Fprint(w, `</channel></rss>`)
				}
			}))
			defer srv.Close()
			base = srv.URL
			dir := t.TempDir()
			config := filepath.Join(dir, "feeds.json")
			state := filepath.Join(dir, "state.json")
			mustWrite(t, config, `[{"name":"テスト","url":"`+base+`","mode":"all"}]`)
			if e := run(context.Background(), config, state, filepath.Join(dir, "dry"), true); e != nil {
				t.Fatal(e)
			}
			if _, e := os.Stat(state); !os.IsNotExist(e) {
				t.Fatal("dry-runで状態を作成しました")
			}
			var m Manifest
			b, _ := os.ReadFile(filepath.Join(dir, "dry", "manifest.json"))
			_ = json.Unmarshal(b, &m)
			if len(m.Articles) != 2 || m.Articles[0].Title != "記事4" {
				t.Fatal(m)
			}
			// 初期化時の除外と、投稿済み最新2件を再現する。
			s, _ := loadState(state)
			s.Initialized[hash(base+"/")] = true
			for i := 1; i <= 4; i++ {
				s.Processed[hash(fmt.Sprintf("%s/%d", base, i))] = time.Now()
			}
			if e := writeJSON(state, s); e != nil {
				t.Fatal(e)
			}
			n = 5
			if e := run(context.Background(), config, state, filepath.Join(dir, "second"), true); e != nil {
				t.Fatal(e)
			}
			b, _ = os.ReadFile(filepath.Join(dir, "second", "manifest.json"))
			_ = json.Unmarshal(b, &m)
			if len(m.Articles) != 1 || m.Articles[0].Title != "記事5" {
				t.Fatalf("古い公開日の新規記事を検出できません: %+v", m)
			}
		})
	}
}
func testAI(t *testing.T) (*AI, *int, *int) {
	t.Helper()
	decisions, summaries := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/decisions":
			decisions++
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "本文") {
				t.Error("判定に本文がありません")
			}
			fmt.Fprint(w, `{"answers":[{"type":"score","name":"interest","score":1.5,"confidence":0.9}]}`)
		case "/responses":
			summaries++
			fmt.Fprint(w, `{"id":"resp_test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"## 技術的な詳細\nTLSの接続処理と12msの測定結果。","annotations":[]}]}]}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := &AI{client: openai.NewClient(option.WithBaseURL(srv.URL+"/"), option.WithAPIKey("test"), option.WithMaxRetries(0)), interestModel: "gpt-6-luna", summaryModel: "gpt-6.1-sol", interestPrompt: "非公開判定", summaryPrompt: "非公開要約", threshold: 0.8, levels: []openai.DecisionNewParamsQuestionScoreLevel{{Label: "低"}, {Label: "高"}}}
	return a, &decisions, &summaries
}
func testRunner(t *testing.T, a *AI) *runner {
	t.Helper()
	dir := t.TempDir()
	s, _ := loadState(filepath.Join(dir, "state.json"))
	return &runner{client: &http.Client{Timeout: time.Second * 5}, ai: a, state: s, statePath: filepath.Join(dir, "state.json"), output: filepath.Join(dir, "out"), claimed: map[string]bool{}}
}
func TestModesDedupAndAck(t *testing.T) {
	for _, mode := range []string{"select", "all", "digest"} {
		t.Run(mode, func(t *testing.T) {
			ai, ds, ss := testAI(t)
			r := testRunner(t, ai)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { fmt.Fprint(w, fixtureHTML("詳細", "")) }))
			defer srv.Close()
			u := srv.URL + "/article"
			keys := []string{hash(u)}
			for j := 0; j < 2; j++ {
				if e := r.process(context.Background(), "情報源", mode, u, "タイトル", "", "紹介文", keys); e != nil {
					t.Fatal(e)
				}
			}
			if len(r.manifest.Articles) != 1 || *ss != 1 {
				t.Fatal(r.manifest, *ss)
			}
			if (mode == "select" && *ds != 1) || (mode != "select" && *ds != 0) {
				t.Fatal("モードの判定分岐", *ds)
			}
			id := r.manifest.Articles[0].ID
			if r.state.done([]string{id}) {
				t.Fatal("投稿前に処理済みです")
			}
			if e := r.save(); e != nil {
				t.Fatal(e)
			}
			b, _ := os.ReadFile(filepath.Join(r.output, r.manifest.Articles[0].Path))
			if !strings.Contains(string(b), "# タイトル") || !strings.Contains(string(b), u) {
				t.Fatal(string(b))
			}
			if e := ack(r.statePath, id); e != nil {
				t.Fatal(e)
			}
			s, _ := loadState(r.statePath)
			if !s.done(keys) {
				t.Fatal("ack後も未処理です")
			}
			stateBytes, _ := os.ReadFile(r.statePath)
			for _, secret := range []string{"タイトル", u, "非公開", "score", "採用", "TLS"} {
				if strings.Contains(string(stateBytes), secret) {
					t.Errorf("状態に非公開情報: %s", secret)
				}
			}
		})
	}
	r := testRunner(t, nil)
	a := Article{Title: "PDFのタイトル", URL: "https://example.com/paper.pdf", PDF: true}
	if e := r.emit(a, "", hash(a.URL), ""); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(r.output, r.manifest.Articles[0].Path))
	if string(b) != "# PDFのタイトル\n\n原文URL: https://example.com/paper.pdf\n" {
		t.Fatal(string(b))
	}
}
func TestDigestOneHopAndFailedArticle(t *testing.T) {
	ai, ds, ss := testAI(t)
	r := testRunner(t, ai)
	var target string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/broken":
			http.Error(w, "失敗", 500)
		case "/article":
			fmt.Fprint(w, fixtureHTML("紹介先", `<p>次の記事への参照を紹介しますが追跡してはいけません。<a href="`+target+`/second">さらに別の記事</a></p>`))
		case "/second":
			t.Error("2ホップ目にアクセスしました")
		default:
			http.NotFound(w, q)
		}
	}))
	defer targetServer.Close()
	target = targetServer.URL
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		fmt.Fprint(w, fixtureHTML("ニュースレター", `<p>新しいTLSの実装に関する詳しい技術記事を紹介します。<a href="`+target+`/broken">取得失敗</a></p><p>新しいTLSの実装に関する詳しい技術記事を紹介します。<a href="`+target+`/article">技術記事</a></p><aside class="sponsor"><a href="`+target+`/ad">広告</a></aside>`))
	}))
	defer source.Close()
	// 別ポートの同一ホストも外部情報源として扱えるよう、テスト専用ホスト名を使う。
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "newsletter.example" {
			copyReq := req.Clone(req.Context())
			copyURL := *req.URL
			copyURL.Host = strings.TrimPrefix(source.URL, "http://")
			copyReq.URL = &copyURL
			resp, err := http.DefaultTransport.RoundTrip(copyReq)
			if err == nil {
				resp.Request = req
			}
			return resp, err
		}
		return http.DefaultTransport.RoundTrip(req)
	})}
	u := "http://newsletter.example/news"
	if e := r.process(context.Background(), "Feisty Duck", "digest", u, "本体", "", "", []string{hash(u)}); e != nil {
		t.Fatal(e)
	}
	if *ds != 1 || *ss != 2 || len(r.manifest.Articles) != 2 {
		t.Fatalf("紹介先処理: 判定%d 要約%d 投稿%d", *ds, *ss, len(r.manifest.Articles))
	}
}
func TestChunksAndConfig(t *testing.T) {
	clearAuthEnv(t)
	s := strings.Repeat("日本語とコード\n\n", 20000)
	parts := chunks(s, 48000)
	if strings.Join(parts, "") != s || len(parts) < 2 {
		t.Fatal("本文の分割に欠落があります")
	}
	for _, key := range []string{"OPENAI_API_KEY", "INTEREST_PROMPT", "SUMMARY_PROMPT", "INTEREST_PROMPT_FILE", "SUMMARY_PROMPT_FILE"} {
		t.Setenv(key, "")
	}
	if _, e := newAI(); e == nil {
		t.Fatal("Secretsなしで通常実行を受理しました")
	}
	feeds, e := readFeeds("feeds.json")
	if e != nil || len(feeds) != 16 {
		t.Fatal(feeds, e)
	}
	if e := ack(filepath.Join(t.TempDir(), "state.json"), "../x"); e == nil {
		t.Fatal("不正なIDを受理しました")
	}
}
func TestShellSyntax(t *testing.T) {
	if b, e := exec.Command("bash", "-n", "scripts/run.sh").CombinedOutput(); e != nil {
		t.Fatalf("%s: %v", b, e)
	}
}

func TestInitialRetryAndNewItems(t *testing.T) {
	clearAuthEnv(t)
	for k, v := range map[string]string{"OPENAI_API_KEY": "test", "INTEREST_PROMPT": "非公開", "SUMMARY_PROMPT": "非公開"} {
		t.Setenv(k, v)
	}
	n := 4
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		fmt.Fprint(w, `<rss version="2.0"><channel><title>テスト</title>`)
		for i := 1; i <= n; i++ {
			fmt.Fprintf(w, `<item><guid>%d</guid><title>記事%d</title><link>%s/%d.pdf</link><pubDate>%s</pubDate></item>`, i, i, base, i, time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC).Format(time.RFC1123Z))
		}
		fmt.Fprint(w, `</channel></rss>`)
	}))
	defer srv.Close()
	base = srv.URL
	dir := t.TempDir()
	cfg := filepath.Join(dir, "feeds.json")
	state := filepath.Join(dir, "state.json")
	mustWrite(t, cfg, `[{"name":"テスト","url":"`+base+`","mode":"all"}]`)
	readManifest := func(out string) Manifest {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(out, "manifest.json"))
		if e != nil {
			t.Fatal(e)
		}
		var m Manifest
		if e = json.Unmarshal(b, &m); e != nil {
			t.Fatal(e)
		}
		return m
	}
	out := filepath.Join(dir, "first")
	if e := run(context.Background(), cfg, state, out, false); e != nil {
		t.Fatal(e)
	}
	m := readManifest(out)
	if len(m.Articles) != 2 || m.Articles[0].Title != "記事4" {
		t.Fatal(m)
	}
	if e := ack(state, m.Articles[0].ID); e != nil {
		t.Fatal(e)
	}
	n = 5
	out = filepath.Join(dir, "second")
	if e := run(context.Background(), cfg, state, out, false); e != nil {
		t.Fatal(e)
	}
	m = readManifest(out)
	if len(m.Articles) != 2 {
		t.Fatalf("未ackの初回記事と新着だけが対象です: %+v", m)
	}
	for _, a := range m.Articles {
		if a.Title != "記事3" && a.Title != "記事5" {
			t.Fatal(a)
		}
	}
}
func TestShellDelivery(t *testing.T) {
	clearAuthEnv(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "rss-curator")
	if b, e := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("ビルド: %s %v", b, e)
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			root := t.TempDir()
			remote := filepath.Join(root, "remote.git")
			repo := filepath.Join(root, "repo")
			if e := os.MkdirAll(filepath.Join(repo, "scripts"), 0700); e != nil {
				t.Fatal(e)
			}
			script, _ := os.ReadFile("scripts/run.sh")
			if e := os.WriteFile(filepath.Join(repo, "scripts", "run.sh"), script, 0700); e != nil {
				t.Fatal(e)
			}
			command := func(wd string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = wd
				b, e := cmd.CombinedOutput()
				if e != nil {
					t.Fatalf("git %v: %s %v", args, b, e)
				}
				return string(b)
			}
			command(root, "init", "--bare", remote)
			command(repo, "init")
			command(repo, "config", "user.email", "test@example.com")
			command(repo, "config", "user.name", "テスト")
			command(repo, "remote", "add", "origin", remote)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `<rss version="2.0"><channel><title>テスト</title><item><guid>pdf</guid><title>PDF</title><link>https://example.com/article.pdf</link></item></channel></rss>`)
			}))
			defer srv.Close()
			mustWrite(t, filepath.Join(repo, "feeds.json"), `[{"name":"テスト","url":"`+srv.URL+`","mode":"all"}]`)
			command(repo, "add", ".")
			command(repo, "commit", "-m", "初期状態")
			mockDir := filepath.Join(root, "bin")
			_ = os.Mkdir(mockDir, 0700)
			exitCode := "0"
			if fail {
				exitCode = "1"
			}
			if e := os.WriteFile(filepath.Join(mockDir, "notify_slack"), []byte("#!/usr/bin/env bash\ncat > /dev/null\nexit "+exitCode+"\n"), 0700); e != nil {
				t.Fatal(e)
			}
			cmd := exec.Command("bash", "scripts/run.sh")
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PATH="+mockDir+":"+os.Getenv("PATH"), "RSS_CURATOR_BIN="+binary, "OPENAI_API_KEY=test", "INTEREST_PROMPT=非公開判定", "SUMMARY_PROMPT=非公開要約", "NOTIFY_SLACK_TOKEN=非公開token", "NOTIFY_SLACK_CHANNEL_ID=Ctest")
			b, e := cmd.CombinedOutput()
			if (e != nil) != fail {
				t.Fatalf("投稿結果の終了コード: %s %v", b, e)
			}
			raw := command(root, "--git-dir="+remote, "show", "rss-state:state.json")
			var s State
			if e = json.Unmarshal([]byte(raw), &s); e != nil {
				t.Fatal(e)
			}
			if s.done([]string{hash("https://example.com/article.pdf")}) == fail {
				t.Fatalf("投稿結果と処理済み状態が一致しません: %s", raw)
			}
			files := command(root, "--git-dir="+remote, "ls-tree", "--name-only", "rss-state")
			if strings.TrimSpace(files) != "state.json" {
				t.Fatalf("状態ブランチに余分なファイル: %s", files)
			}
		})
	}
}

func TestRejectionAndCanonicalDedup(t *testing.T) {
	ai, ds, ss := testAI(t)
	ai.threshold = 1.9
	r := testRunner(t, ai)
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		fmt.Fprint(w, strings.Replace(fixtureHTML("記事", ""), "</head>", `<link rel="canonical" href="`+base+`/canonical"></head>`, 1))
	}))
	defer srv.Close()
	base = srv.URL
	for _, path := range []string{"/one", "/two"} {
		u := base + path
		if e := r.process(context.Background(), "RSS", "select", u, "記事", "", "", []string{hash(u)}); e != nil {
			t.Fatal(e)
		}
	}
	if *ds != 1 || *ss != 0 || len(r.manifest.Articles) != 0 || !r.state.done([]string{hash(base + "/two")}) {
		t.Fatal("不採用またはcanonicalによる重複排除が不正です")
	}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	s, e := loadState(r.statePath)
	if e != nil || !s.done([]string{hash(base + "/one")}) {
		t.Fatal("不採用の記事を永続化できません")
	}
}
func TestLiveExtraction(t *testing.T) {
	if os.Getenv("RSS_CURATOR_LIVE_TEST") != "1" {
		t.Skip("実サイト確認は明示的に有効化した場合のみ実行します")
	}
	feeds, e := readFeeds("feeds.json")
	if e != nil {
		t.Fatal(e)
	}
	client := &http.Client{Timeout: 45 * time.Second}
	for _, f := range feeds {
		if f.Name != "Cloudflare" && f.Name != "Meta Engineering" && f.Name != "The Go Blog" && f.Name != "Feisty Duck" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			feed, e := fetchFeed(context.Background(), client, f.URL)
			if e != nil || len(feed.Items) == 0 {
				t.Fatal("RSSを取得できません", e)
			}
			a, e := fetchArticle(context.Background(), client, feed.Items[0].Link, feed.Items[0].Title, "")
			if e != nil {
				t.Fatal(e)
			}
			if a.Markdown == "" {
				t.Fatal("本文が空です")
			}
			t.Logf("本文抽出: %dバイト、紹介先%d件", len(a.Markdown), len(digestLinks(a)))
		})
	}
}

func TestDigestSkippedBaseline(t *testing.T) {
	r := testRunner(t, nil)
	u := "https://example.com/old-newsletter"
	r.state.Processed[hash(u)] = time.Now()
	if e := r.process(context.Background(), "Feisty Duck", "digest", u, "古い本体", "", "", []string{hash(u)}); e != nil {
		t.Fatal(e)
	}
	if len(r.manifest.Articles) != 0 {
		t.Fatal("初期化対象外の本体をバックフィルしました")
	}
}

func TestAPIErrorsDoNotExposeInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"非公開プロンプトとAPIキーを含むエラー","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	a := &AI{client: openai.NewClient(option.WithBaseURL(srv.URL+"/"), option.WithAPIKey("secret"), option.WithMaxRetries(0)), interestModel: "gpt-6-luna", summaryModel: "gpt-6.1-sol"}
	article := Article{Title: "テスト", URL: "https://example.com/", Markdown: "本文"}
	_, interestErr := a.interest(context.Background(), article, "")
	_, summaryErr := a.summarize(context.Background(), article)
	for _, e := range []error{interestErr, summaryErr} {
		if e == nil || !strings.Contains(e.Error(), "401") || strings.Contains(e.Error(), "非公開") || strings.Contains(e.Error(), "secret") {
			t.Fatalf("APIのエラーが不適切です: %v", e)
		}
	}
}
