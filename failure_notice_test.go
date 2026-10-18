package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTerminalFetchFailureNotice(t *testing.T) {
	for _, scenario := range []string{"permanent", "limit", "crawlbase", "retry", "excluded"} {
		t.Run(scenario, func(t *testing.T) {
			calls, posts, checkpoints := 0, 0, 0
			r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if scenario == "crawlbase" {
					return fetchResponse(req, 520, "", map[string]string{"cb_status": "705", "original_status": "403"}), nil
				}
				status := 404
				if scenario == "limit" || scenario == "retry" {
					status = 503
				}
				return fetchResponse(req, status, "", nil), nil
			})
			raw := "https://site.example/article?token=SECRET"
			if scenario == "excluded" {
				raw = "https://youtu.be/test"
			}
			id := hash(raw)
			if scenario == "limit" {
				r.maxFetchFailures = 1
			}
			if scenario == "crawlbase" {
				r.state.Fetch[id] = FetchState{Method: fetchCrawlbase}
			}
			r.post = func(noticeID, path string) error {
				posts++
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), "記事の取得を断念") || strings.Contains(string(b), "SECRET") || noticeID == id {
					t.Fatal(string(b), err)
				}
				s, err := loadState(r.statePath)
				if err != nil || !s.Fetch[id].FailureNoticePending {
					t.Fatal("通知前に待機状態を保存していません", err)
				}
				return nil
			}
			r.checkpoint = func() error { checkpoints++; return nil }
			_ = r.process(context.Background(), "source", "select", raw, "記事", "", "", []string{id})
			want := 1
			if scenario == "retry" || scenario == "excluded" {
				want = 0
			}
			if posts != want || checkpoints != want || r.state.Fetch[id].FailureNoticePending {
				t.Fatal(posts, checkpoints, r.state.Fetch[id])
			}
			if scenario == "retry" {
				return
			}
			before := calls
			next := restartFetcher(t, r, time.Hour)
			if err := next.process(context.Background(), "source", "select", raw, "記事", "", "", []string{id}); err != nil || posts != want || calls != before {
				t.Fatal("再通知または再取得", err, posts, calls)
			}
		})
	}
}

func TestFailureNoticeRetryWithoutArticleRequest(t *testing.T) {
	calls, posts := 0, 0
	r := fetchTestRunner(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return fetchResponse(req, 410, "", nil), nil
	})
	raw := "https://site.example/gone"
	id := hash(raw)
	r.post = func(_, _ string) error { posts++; return fmt.Errorf("Slack failure") }
	if err := r.process(context.Background(), "source", "select", raw, "", "", "", []string{id}); !errors.Is(err, errDelivery) {
		t.Fatal(err)
	}
	next := restartFetcher(t, r, time.Hour)
	if !next.state.Fetch[id].FailureNoticePending {
		t.Fatal("通知待ちが失われました")
	}
	next.post = func(_, _ string) error { posts++; return nil }
	marker := hash("digest\nparent")
	next.state.Processed["parent"] = next.fetchTime()
	next.rememberDigest(marker, []Link{{URL: raw}})
	if !next.digestNeedsFetch(marker, []string{"parent"}) {
		t.Fatal("通知待ちを完了扱いしました")
	}
	if err := next.process(context.Background(), "source", "select", raw, "", "", "", []string{id}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || posts != 2 || next.state.Fetch[id].FailureNoticePending {
		t.Fatal(calls, posts)
	}
	if next.digestNeedsFetch(marker, []string{"parent"}) {
		t.Fatal("通知済みを再処理します")
	}
}
