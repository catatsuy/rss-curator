package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCompletedDigestNeverRefetches(t *testing.T) {
	parentCalls, childCalls := 0, 0
	parent := "https://newsletter.example/issue"
	child := "https://article.example/gone"
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "newsletter.example" {
			parentCalls++
			return fetchResponse(req, 200, fixtureHTML("ニュースレター", `<p>この記事はTLSの接続処理と暗号プロトコルの具体的な実装および測定結果について詳しく解説しています。<a href="`+child+`">詳細記事</a></p>`), nil), nil
		}
		childCalls++
		return fetchResponse(req, 404, "", nil), nil
	})
	ai, ds, ss := testAI(t)
	r.ai = ai
	posts := 0
	r.post = func(string, string) error { posts++; return nil }
	keys := []string{hash(parent)}
	marker := hash("digest\n" + keys[0])
	if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", keys); e != nil {
		t.Fatal(e)
	}
	if parentCalls != 2 || childCalls != 2 || posts != 1 || *ss != 1 || *ds != 0 {
		t.Fatal(parentCalls, childCalls, posts, *ss, *ds)
	}
	if _, complete := r.state.Processed[marker]; !complete {
		t.Fatal("紹介先の完了を保存していません")
	}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	for _, advance := range []time.Duration{6 * time.Hour, 40 * 24 * time.Hour} {
		r = restartFetcher(t, r, advance)
		if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", keys); e != nil {
			t.Fatal(e)
		}
	}
	if parentCalls != 2 || childCalls != 2 || posts != 1 || *ss != 1 {
		t.Fatal("完了済み本体への無駄なリクエスト", parentCalls, childCalls, posts, *ss)
	}
}

func TestDigestSkipsWhenChildrenCannotRetry(t *testing.T) {
	for _, reason := range []string{"retry_wait", "no_token", "budget", "api_cooldown", "failure_limit", "completed"} {
		t.Run(reason, func(t *testing.T) {
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				t.Fatal("再試行不要な本体を取得しました")
				return nil, nil
			})
			parent := "https://newsletter.example/issue"
			child := "https://article.example/article"
			id := hash(parent)
			childID := hash(child)
			marker := hash("digest\n" + id)
			r.state.Processed[id] = r.fetchTime()
			r.state.Aliases[marker] = id
			r.rememberDigest(marker, []Link{{Title: "記事", URL: child, Context: "非公開にする紹介文"}})
			f := FetchState{Method: fetchCrawlbase, Failures: 1, Attempts: 1, LastFailure: "http_403", LastSeen: r.fetchTime()}
			switch reason {
			case "retry_wait":
				f.NextAttempt = r.fetchTime().Add(time.Hour)
			case "no_token":
				r.crawlbase.token = ""
			case "budget":
				r.crawlbase.used = r.crawlbase.limit
			case "api_cooldown":
				r.state.CrawlbaseRetryAfter = r.fetchTime().Add(time.Hour)
			case "failure_limit":
				f.Failures = 3
				f.Attempts = 3
			case "completed":
				r.state.Processed[childID] = r.fetchTime()
			}
			r.state.Fetch[childID] = f
			if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil {
				t.Fatal(e)
			}
			if e := r.save(); e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(r.state)
			for _, private := range []string{parent, child, "非公開にする紹介文"} {
				if strings.Contains(string(b), private) {
					t.Fatal("状態にURLや紹介文を保存しました")
				}
			}
			complete := reason == "failure_limit" || reason == "completed"
			_, got := r.state.Processed[marker]
			if got != complete {
				t.Fatal("完了判定が不正", reason, got)
			}
			if !complete && r.state.done([]string{childID}) {
				t.Fatal("待機中の紹介先を失いました")
			}
		})
	}
}

func TestDigestResumesOnlyWhenChildDue(t *testing.T) {
	parent := "https://newsletter.example/issue"
	child := "https://article.example/article"
	normal, cb := 0, 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crawlbase.com" {
			cb++
			return goodCrawlResponse(req, fixtureHTML("記事", "")), nil
		}
		if req.URL.Hostname() != "newsletter.example" {
			t.Fatal("Crawlbaseの予約を通常HTTPで取得しました")
		}
		normal++
		return fetchResponse(req, 200, fixtureHTML("ニュースレター", fmt.Sprintf(`<p>この記事はTLSの接続処理と暗号プロトコルの具体的な実装および測定結果について詳しく解説しています。<a href="%s">詳細記事</a></p>`, child)), nil), nil
	})
	ai, _, _ := testAI(t)
	r.ai = ai
	r.post = func(string, string) error { return nil }
	id := hash(parent)
	marker := hash("digest\n" + id)
	childID := hash(child)
	r.state.Processed[id] = r.fetchTime()
	r.state.Aliases[marker] = id
	r.rememberDigest(marker, []Link{{URL: child}})
	r.state.Fetch[childID] = FetchState{Method: fetchCrawlbase, Failures: 1, Attempts: 1, LastFailure: "http_403", LastSeen: r.fetchTime(), NextAttempt: r.fetchTime().Add(time.Hour)}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	r = restartFetcher(t, r, 30*time.Minute)
	if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil || normal != 0 || cb != 0 {
		t.Fatal("待機中に本体へアクセスしました", e, normal, cb)
	}
	r = restartFetcher(t, r, time.Hour)
	if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil || normal != 2 || cb != 1 {
		t.Fatal("必要な再試行ができません", e, normal, cb)
	}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	r = restartFetcher(t, r, 6*time.Hour)
	if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil || normal != 2 || cb != 1 {
		t.Fatal("完了後もアクセスしました", e, normal, cb)
	}
}

func TestLegacyDigestDiscoversOnce(t *testing.T) {
	calls := 0
	parent := "https://newsletter.example/issue"
	child := "https://article.example/article"
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "newsletter.example" {
			t.Fatal("投稿済み紹介先を再取得しました")
		}
		calls++
		return fetchResponse(req, 200, fixtureHTML("ニュースレター", fmt.Sprintf(`<p>この記事はTLSの接続処理と暗号プロトコルの具体的な実装および測定結果について詳しく解説しています。<a href="%s">詳細記事</a></p>`, child)), nil), nil
	})
	id := hash(parent)
	r.state.Processed[id] = r.fetchTime()
	r.state.Processed[hash(child)] = r.fetchTime()
	r.state.Aliases[hash("digest\n"+id)] = id
	for i := 0; i < 3; i++ {
		if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil {
			t.Fatal(e)
		}
		if e := r.save(); e != nil {
			t.Fatal(e)
		}
		r = restartFetcher(t, r, 6*time.Hour)
	}
	if calls != 2 {
		t.Fatal("旧状態の移行で何度も本体を取得しました", calls)
	}
}

func TestDigestRetainsWaitingChildState(t *testing.T) {
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		t.Fatal("待機中に通信しました")
		return nil, nil
	})
	parent := "https://newsletter.example/issue"
	id := hash(parent)
	childID := hash("https://article.example/article")
	marker := hash("digest\n" + id)
	r.state.Processed[id] = r.fetchTime()
	r.state.Aliases[marker] = id
	r.state.Digests = map[string]DigestState{marker: {Items: []string{childID}, LastSeen: r.fetchTime()}}
	r.state.Fetch[childID] = FetchState{Method: fetchCrawlbase, Attempts: 1, Failures: 1, LastFailure: "http_429", LastSeen: r.fetchTime().Add(-29 * 24 * time.Hour), NextAttempt: r.fetchTime().Add(90 * 24 * time.Hour)}
	if e := r.process(context.Background(), "newsletter", "digest", parent, "本体", "", "", []string{id}); e != nil {
		t.Fatal(e)
	}
	if e := r.save(); e != nil {
		t.Fatal(e)
	}
	next := restartFetcher(t, r, 2*24*time.Hour)
	next.state.pruneFetch(next.fetchTime())
	if next.state.Fetch[childID].Method != fetchCrawlbase {
		t.Fatal("再発見されている待機状態を削除しました")
	}
}
