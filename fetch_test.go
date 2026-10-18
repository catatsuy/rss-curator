package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fetchResponse(req *http.Request, status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}
func fetchTestRunner(t *testing.T, transport roundTripFunc) *runner {
	t.Helper()
	r := testRunner(t, nil)
	r.client = &http.Client{Transport: transport, Timeout: time.Second}
	r.now = func() time.Time { return time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC) }
	r.crawlbase = &crawlbaseClient{client: r.client, token: "private-crawlbase-token", limit: 5}
	return r
}
func restartFetcher(t *testing.T, r *runner, advance time.Duration) *runner {
	t.Helper()
	s, e := loadState(r.statePath)
	if e != nil {
		t.Fatal(e)
	}
	next := *r
	next.state = s
	next.fetchAttempted = nil
	next.claimed = map[string]bool{}
	next.crawlbase = &crawlbaseClient{client: r.crawlbase.client, token: r.crawlbase.token, limit: r.crawlbase.limit}
	now := r.fetchTime().Add(advance)
	next.now = func() time.Time { return now }
	return &next
}
func goodCrawlResponse(req *http.Request, body string) *http.Response {
	return fetchResponse(req, 200, body, map[string]string{"cb_status": "200", "original_status": "200", "url": req.URL.Query().Get("url")})
}

func TestFetchHTTP200AndUnconfigured(t *testing.T) {
	t.Setenv("CRAWLBASE_TOKEN", "")
	for _, token := range []string{"", "private-crawlbase-token"} {
		t.Run(fmt.Sprint(token == ""), func(t *testing.T) {
			calls := 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "api.crawlbase.com" {
					t.Fatal("通常HTTP成功でCrawlbaseを使用しました")
				}
				calls++
				return fetchResponse(req, 200, fixtureHTML("記事", ""), nil), nil
			})
			r.crawlbase.token = token
			a, e := r.fetchArticle(context.Background(), "https://site.example/article", "記事", "")
			if e != nil || !strings.Contains(a.Markdown, "12ms") || calls != 2 || r.crawlbase.used != 0 {
				t.Fatal(a, e, calls)
			}
		})
	}
}

func TestFetch403NextRunAndProcessing(t *testing.T) {
	normal, cb := 0, 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crawlbase.com" {
			cb++
			if req.Method != "GET" || req.URL.Query().Get("javascript") != "" || req.URL.Query().Get("token") != "private-crawlbase-token" {
				t.Error("Normal TokenのGET以外を使用")
			}
			return goodCrawlResponse(req, fixtureHTML("記事", `<p><a href="/more">相対リンク</a></p>`)), nil
		}
		normal++
		return fetchResponse(req, 403, "refused", nil), nil
	})
	u := "https://site.example/article"
	id := hash(u)
	if _, e := r.fetchArticle(context.Background(), u, "記事", ""); e == nil {
		t.Fatal("403を成功扱い")
	}
	if cb != 0 || r.state.Fetch[id].Method != fetchCrawlbase || r.state.done([]string{id}) {
		t.Fatal("次回予約が不正")
	}
	if _, e := r.fetchArticle(context.Background(), u, "記事", ""); !errors.Is(e, errFetchDeferred) || cb != 0 {
		t.Fatal("同じ実行でCrawlbaseを使用", e)
	}
	next := restartFetcher(t, r, 0)
	ai, ds, ss := testAI(t)
	next.ai = ai
	next.post = func(_, path string) error {
		b, e := os.ReadFile(path)
		if e != nil || !strings.Contains(string(b), "技術的な詳細") {
			t.Error("通常の要約・投稿に到達しません", e)
		}
		return nil
	}
	if e := next.process(context.Background(), "source", "select", u, "記事", "", "紹介文", []string{id}); e != nil {
		t.Fatal(e)
	}
	if normal != 2 || cb != 1 || *ds != 1 || *ss != 1 || !next.state.done([]string{id}) {
		t.Fatal(normal, cb, *ds, *ss)
	}
	if _, ok := next.state.Fetch[id]; ok {
		t.Fatal("処理完了した取得状態が残っています")
	}
	if e := next.process(context.Background(), "source", "select", u, "記事", "", "", []string{id}); e != nil || cb != 1 {
		t.Fatal("投稿済み記事を再取得", e)
	}
}

func TestFetch429RetryAfterAndHostCooldown(t *testing.T) {
	for _, value := range []string{"7200", "Sun, 18 Oct 2026 03:00:00 GMT", "", "invalid", "1"} {
		t.Run(value, func(t *testing.T) {
			normal, cb := 0, 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "api.crawlbase.com" {
					cb++
					return goodCrawlResponse(req, fixtureHTML("記事", "")), nil
				}
				normal++
				return fetchResponse(req, 429, "limited", map[string]string{"Retry-After": value}), nil
			})
			u := "https://site.example/article"
			id := hash(u)
			_, e := r.fetchArticle(context.Background(), u, "記事", "")
			if e == nil || normal != 1 || cb != 0 {
				t.Fatal("HEADの429の後も取得しました", e, normal, cb)
			}
			wait := time.Hour
			if value == "7200" || strings.Contains(value, "03:00") {
				wait = 2 * time.Hour
			}
			until := r.fetchTime().Add(wait)
			if !r.state.Fetch[id].NextAttempt.Equal(until) || r.state.Fetch[id].Method != fetchCrawlbase {
				t.Fatal(r.state.Fetch[id])
			}
			_, e = r.fetchArticle(context.Background(), "https://site.example/other", "別記事", "")
			if !errors.Is(e, errFetchDeferred) || normal != 1 {
				t.Fatal("同じサイトへの取得を続けました", e)
			}
			// 新しいプロセスでもホスト待機と記事の待機を維持する。
			if e = r.save(); e != nil {
				t.Fatal(e)
			}
			next := restartFetcher(t, r, wait-time.Second)
			_, e = next.fetchArticle(context.Background(), u, "記事", "")
			if !errors.Is(e, errFetchDeferred) || cb != 0 || next.state.done([]string{id}) {
				t.Fatal("待機記事を失いました", e)
			}
			next = restartFetcher(t, r, wait)
			if _, e = next.fetchArticle(context.Background(), u, "記事", ""); e != nil || cb != 1 || normal != 1 {
				t.Fatal("期限後のCrawlbase取得", e, cb, normal)
			}
		})
	}
}

func TestFetchHTTPFailurePolicies(t *testing.T) {
	for _, status := range []int{401, 404, 410, 500, 503, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls, cb := 0, 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "api.crawlbase.com" {
					cb++
					t.Fatal("Crawlbase対象でない失敗です")
				}
				calls++
				if status == 0 {
					return nil, context.DeadlineExceeded
				}
				return fetchResponse(req, status, "failure", nil), nil
			})
			u := "https://site.example/article"
			id := hash(u)
			if _, e := r.fetchArticle(context.Background(), u, "記事", ""); e == nil {
				t.Fatal("失敗を成功扱い")
			}
			firstCalls := calls
			next := restartFetcher(t, r, 6*time.Hour)
			permanent := status == 401 || status == 404 || status == 410
			if permanent {
				if !next.state.done([]string{id}) || !next.state.Fetch[id].Stopped {
					t.Fatal("恒久失敗を保存しません")
				}
				if e := next.process(context.Background(), "source", "select", u, "記事", "", "", []string{id}); e != nil || calls != firstCalls {
					t.Fatal("恒久失敗を再取得", e)
				}
			} else {
				if next.state.Fetch[id].Method != fetchHTTP || next.state.done([]string{id}) {
					t.Fatal("一時失敗の取得方法が不正")
				}
				_, _ = next.fetchArticle(context.Background(), u, "記事", "")
				if calls != firstCalls*2 {
					t.Fatal("次回に通常HTTPで再試行しません", calls)
				}
			}
			if cb != 0 {
				t.Fatal(cb)
			}
		})
	}
}

func TestCrawlbaseFailureBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		http, cb, original int
		reason             string
		attempts           int
	}{
		{"service", 503, 0, 0, "crawlbase_service_error", 3},
		{"service429", 429, 0, 0, "crawlbase_rate_limited", 3},
		{"upstream", 520, 599, 0, "crawlbase_upstream_error", 3},
		{"target5xx", 200, 704, 503, "target_server_error", 3},
		{"missing_metadata", 200, 0, 0, "crawlbase_upstream_error", 3},
		{"target403", 520, 705, 403, "target_forbidden", 1},
		{"target429", 200, 706, 429, "target_rate_limited", 1},
		{"target404", 200, 404, 404, "target_permanent", 1},
		{"challenge", 200, 503, 200, "target_blocked", 1},
		{"empty", 200, 207, 200, "target_empty", 1},
		{"configuration", 401, 0, 0, "crawlbase_configuration", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() != "api.crawlbase.com" {
					t.Fatal("予約した記事を通常HTTPで取得しました")
				}
				return fetchResponse(req, tc.http, "private-crawlbase-token", map[string]string{"cb_status": fmt.Sprint(tc.cb), "original_status": fmt.Sprint(tc.original)}), nil
			})
			u := "https://site.example/article"
			id := hash(u)
			r.state.Fetch[id] = FetchState{Method: fetchCrawlbase, LastSeen: r.fetchTime()}
			for i := 1; i <= tc.attempts; i++ {
				_, e := r.fetchArticle(context.Background(), u, "記事", "")
				if e == nil || strings.Contains(e.Error(), "private-crawlbase-token") {
					t.Fatal("失敗または機密の扱いが不正", e)
				}
				f := r.state.Fetch[id]
				if f.CrawlbaseAttempts != i || f.LastFailure != tc.reason || f.Stopped != (i == tc.attempts) {
					t.Fatal(i, f)
				}
				if _, e = r.fetchArticle(context.Background(), u, "記事", ""); !errors.Is(e, errFetchDeferred) {
					t.Fatal("同じ実行内の再試行", e)
				}
				r = restartFetcher(t, r, 6*time.Hour)
			}
			_, e := r.fetchArticle(context.Background(), u, "記事", "")
			if !errors.Is(e, errFetchDeferred) || !r.state.done([]string{id}) || r.crawlbase.used != 0 {
				t.Fatal("上限後も再試行しました", e)
			}
		})
	}
}

func TestCrawlbaseBudgetAndMissingToken(t *testing.T) {
	calls := 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return goodCrawlResponse(req, fixtureHTML("記事", "")), nil
	})
	for i := 0; i < 6; i++ {
		u := fmt.Sprintf("https://site.example/%d", i)
		id := hash(u)
		original := FetchState{Method: fetchCrawlbase, LastFailure: "http_403", Attempts: 1, LastSeen: r.fetchTime()}
		r.state.Fetch[id] = original
		_, e := r.fetchArticle(context.Background(), u, "記事", "")
		if i < 5 && e != nil {
			t.Fatal(e)
		}
		if i == 5 && (!errors.Is(e, errFetchDeferred) || r.state.Fetch[id] != original || r.state.done([]string{id})) {
			t.Fatal("上限到達を失敗または完了として記録", e, r.state.Fetch[id])
		}
	}
	if calls != 5 || r.crawlbase.used != 5 {
		t.Fatal(calls, r.crawlbase.used)
	}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	r = restartFetcher(t, r, 6*time.Hour)
	u := "https://site.example/5"
	r.crawlbase.token = ""
	if _, e := r.fetchArticle(context.Background(), u, "記事", ""); !errors.Is(e, errFetchDeferred) || calls != 5 {
		t.Fatal("未設定時にCrawlbaseを呼びました", e)
	}
	r.crawlbase.token = "private-crawlbase-token"
	if _, e := r.fetchArticle(context.Background(), u, "記事", ""); e != nil || calls != 6 {
		t.Fatal("次回へ繰り越しできません", e)
	}
}

func TestCrawlbaseConfigurationAndPruning(t *testing.T) {
	t.Setenv("CRAWLBASE_MAX_REQUESTS", "")
	c, e := newCrawlbaseClient(&http.Client{})
	if e != nil || c.limit != 5 {
		t.Fatal(c, e)
	}
	for _, v := range []string{"0", "2", "-1", "bad"} {
		t.Setenv("CRAWLBASE_MAX_REQUESTS", v)
		c, e = newCrawlbaseClient(&http.Client{})
		if (e != nil) != (v == "-1" || v == "bad") {
			t.Fatal(v, c, e)
		}
	}
	s, e := loadState(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	s.Fetch["old"] = FetchState{Method: fetchCrawlbase, LastSeen: now.Add(-fetchRetention - time.Second)}
	s.Fetch["active"] = FetchState{Method: fetchCrawlbase, LastSeen: now}
	s.HostRetryAfter[hash("expired.example")] = now.Add(-time.Second)
	s.pruneFetch(now)
	if len(s.Fetch) != 1 || s.Fetch["active"].Method != fetchCrawlbase || len(s.HostRetryAfter) != 0 {
		t.Fatal(s)
	}
	b, _ := json.Marshal(s)
	var legacy State
	if e = json.Unmarshal([]byte(`{"processed":{},"initialized":{},"aliases":{}}`), &legacy); e != nil {
		t.Fatal(e)
	}
	legacy.pruneFetch(now)
	if legacy.Fetch == nil || legacy.HostRetryAfter == nil || strings.Contains(string(b), "private-crawlbase-token") {
		t.Fatal("互換性または秘密情報の保存")
	}
}

func TestCrawlbaseMetadataURLAndLogSafety(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crawlbase.com" {
			resp := goodCrawlResponse(req, strings.Replace(fixtureHTML("記事", ""), "</head>", `<link rel="canonical" href="/canonical"></head>`, 1))
			resp.Header.Set("url", "https://site.example/final")
			return resp, nil
		}
		return fetchResponse(req, 403, "", nil), nil
	})
	u := "https://site.example/article?token=private-crawlbase-token&password=PRIVATE_PASSWORD"
	_, _ = r.fetchArticle(context.Background(), u, "記事", "")
	r = restartFetcher(t, r, 0)
	a, e := r.fetchArticle(context.Background(), u, "記事", "")
	if e != nil || a.URL != "https://site.example/canonical" || !strings.Contains(a.Markdown, "https://site.example/reference") {
		t.Fatal("本文抽出の基準URLが不正", a, e)
	}
	b, _ := os.ReadFile(r.statePath)
	for _, private := range []string{"private-crawlbase-token", "PRIVATE_PASSWORD", "api.crawlbase.com/?"} {
		if strings.Contains(logs.String(), private) || strings.Contains(string(b), private) {
			t.Fatal("ログまたは状態に認証情報", private)
		}
	}
	if !strings.Contains(logs.String(), `"cb_status":200`) || !strings.Contains(logs.String(), `"method":"crawlbase"`) {
		t.Fatal("構造化ログがありません", logs.String())
	}
	// transportエラーにトークンを含むURLがあっても公開しない。
	r.crawlbase.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("private-crawlbase-token %s", req.URL)
	})}
	_, _, e = r.crawlbase.fetch(context.Background(), u, "記事")
	if e == nil || strings.Contains(e.Error(), "private-crawlbase-token") {
		t.Fatal(e)
	}
}

func TestCrawlbaseDoesNotFollowAPIRedirect(t *testing.T) {
	t.Setenv("CRAWLBASE_TOKEN", "private-crawlbase-token")
	calls := 0
	c, e := newCrawlbaseClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return fetchResponse(req, 302, "", map[string]string{"Location": "https://other.example/?token=private-crawlbase-token"}), nil
	})})
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = c.fetch(context.Background(), "https://site.example/article", "記事")
	if e == nil || calls != 1 || strings.Contains(e.Error(), "private-crawlbase-token") {
		t.Fatal(e, calls)
	}
}

func TestFetchStateAcrossShellRuns(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("CRAWLBASE_TOKEN", "")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	binary := filepath.Join(root, "rss-curator")
	if b, e := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	if e := os.MkdirAll(filepath.Join(repo, "scripts"), 0700); e != nil {
		t.Fatal(e)
	}
	script, e := os.ReadFile("scripts/run.sh")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(repo, "scripts/run.sh"), script, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %s %v", args, b, e)
		}
		return string(b)
	}
	git(root, "init", "--bare", remote)
	git(repo, "init")
	git(repo, "config", "user.email", "test@example.com")
	git(repo, "config", "user.name", "test")
	git(repo, "remote", "add", "origin", remote)
	// 実APIを呼ばず、RSSと通常HTTPの403を同じローカルサーバーで再現。
	feedCalls, articleCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/feed" {
			feedCalls++
			fmt.Fprintf(w, `<rss version="2.0"><channel><title>test</title><item><guid>article</guid><title>article</title><link>http://%s/article</link></item></channel></rss>`, req.Host)
			return
		}
		articleCalls++
		w.WriteHeader(403)
	}))
	defer server.Close()
	mustWrite(t, filepath.Join(repo, "feeds.json"), `[{"name":"test","url":"`+server.URL+`/feed","mode":"select"}]`)
	git(repo, "add", ".")
	git(repo, "commit", "-m", "initial")
	mockDir := filepath.Join(root, "bin")
	if e = os.Mkdir(mockDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(mockDir, "notify_slack"), []byte("#!/usr/bin/env bash\nexit 99\n"), 0700); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		cmd := exec.Command("bash", "scripts/run.sh")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "PATH="+mockDir+":"+os.Getenv("PATH"), "RSS_CURATOR_BIN="+binary, "OPENAI_API_KEY=test", "INTEREST_PROMPT=test", "SUMMARY_PROMPT=test", "NOTIFY_SLACK_TOKEN=test", "NOTIFY_SLACK_CHANNEL_ID=Ctest")
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("run %d: %v %s", i, e, b)
		}
	}
	if feedCalls != 2 || articleCalls != 2 {
		t.Fatal("次の実行で通常HTTPを再試行しました", feedCalls, articleCalls)
	}
	raw := git(root, "--git-dir="+remote, "show", "rss-state:state.json")
	path := filepath.Join(root, "restored-state.json")
	mustWrite(t, path, raw)
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "api.crawlbase.com" {
			t.Fatal("復元した予約を通常HTTPで取得しました")
		}
		return goodCrawlResponse(req, fixtureHTML("記事", "")), nil
	})
	r.statePath = path
	r.state, e = loadState(path)
	if e != nil {
		t.Fatal(e)
	}
	r.now = nil
	a, e := r.fetchArticle(context.Background(), server.URL+"/article", "記事", "")
	if e != nil || a.Markdown == "" || r.crawlbase.used != 1 {
		t.Fatal("状態ブランチからの復元失敗", e)
	}
}

func TestCrawlbaseServiceCooldownAndDisable(t *testing.T) {
	for _, status := range []int{429, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				calls++
				return fetchResponse(req, status, "", map[string]string{"Retry-After": "7200"}), nil
			})
			for _, u := range []string{"https://a.example/article", "https://b.example/article"} {
				r.state.Fetch[hash(u)] = FetchState{Method: fetchCrawlbase, LastSeen: r.fetchTime()}
			}
			_, _ = r.fetchArticle(context.Background(), "https://a.example/article", "記事", "")
			if _, e := r.fetchArticle(context.Background(), "https://b.example/article", "記事", ""); !errors.Is(e, errFetchDeferred) || calls != 1 {
				t.Fatal("APIの制限後も後続記事を取得", e, calls)
			}
			if status == 429 {
				if e := r.save(); e != nil {
					t.Fatal(e)
				}
				next := restartFetcher(t, r, time.Hour)
				if _, e := next.fetchArticle(context.Background(), "https://b.example/article", "記事", ""); !errors.Is(e, errFetchDeferred) || calls != 1 {
					t.Fatal("APIの待機期限が実行間で失われました", e, calls)
				}
			}
		})
	}
}

func TestCrawlbaseZeroBudget(t *testing.T) {
	t.Setenv("CRAWLBASE_TOKEN", "private-crawlbase-token")
	t.Setenv("CRAWLBASE_MAX_REQUESTS", "0")
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		t.Fatal("上限0で取得しました")
		return nil, nil
	})
	var e error
	r.crawlbase, e = newCrawlbaseClient(r.client)
	if e != nil {
		t.Fatal(e)
	}
	u := "https://site.example/article"
	id := hash(u)
	f := FetchState{Method: fetchCrawlbase, LastSeen: r.fetchTime()}
	r.state.Fetch[id] = f
	if _, e = r.fetchArticle(context.Background(), u, "記事", ""); !errors.Is(e, errFetchDeferred) || r.state.Fetch[id].Attempts != 0 {
		t.Fatal("上限0を失敗扱い", e)
	}
}

func TestPostedDigestKeepsFetchRetry(t *testing.T) {
	normal, cb := 0, 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crawlbase.com" {
			cb++
			return goodCrawlResponse(req, fixtureHTML("ニュースレター", "")), nil
		}
		normal++
		return fetchResponse(req, 403, "", nil), nil
	})
	u := "https://newsletter.example/article"
	id := hash(u)
	r.state.Processed[id] = r.fetchTime().Add(-24 * time.Hour)
	r.state.Aliases[hash("digest\n"+id)] = id
	if e := r.process(context.Background(), "newsletter", "digest", u, "記事", "", "", []string{id}); e == nil {
		t.Fatal("通常HTTPの403がありません")
	}
	if r.state.Fetch[id].Method != fetchCrawlbase {
		t.Fatal("投稿済み本体の取得予約が消えました")
	}
	next := restartFetcher(t, r, 6*time.Hour)
	// 投稿済み本体は要約・再投稿せず、取得だけ再開できる。
	if e := next.process(context.Background(), "newsletter", "digest", u, "記事", "", "", []string{id}); e != nil || cb != 1 || normal != 2 || len(next.manifest.Articles) != 0 {
		t.Fatal("投稿済みdigestの再取得が不正", e, normal, cb)
	}
}
