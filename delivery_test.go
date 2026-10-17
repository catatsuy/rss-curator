package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestImmediateDelivery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			ai, _, summaries := testAI(t)
			r := testRunner(t, ai)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				fmt.Fprint(w, fixtureHTML("記事", `<p>技術記事の紹介<a href="https://target.example/article">詳細</a></p>`))
			}))
			defer srv.Close()
			posts, checkpoints := 0, 0
			r.post = func(id, path string) error {
				posts++
				if *summaries != 1 || r.state.done([]string{id}) {
					t.Fatal("要約直後、処理済み記録前に投稿していません")
				}
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), "TLS") {
					t.Fatal("要約を投稿に渡していません", err)
				}
				if fail {
					return fmt.Errorf("Slack失敗")
				}
				return nil
			}
			id := hash(srv.URL + "/article")
			r.checkpoint = func() error {
				checkpoints++
				s, err := loadState(r.statePath)
				if err != nil || !s.done([]string{id}) {
					t.Fatal("永続化前に投稿成功が記録されていません", err)
				}
				return nil
			}
			mode := "all"
			if fail {
				mode = "digest" // 本体の投稿失敗後、紹介先へ進んではいけない。
			}
			err := r.process(context.Background(), "source", mode, srv.URL+"/article", "記事", "", "", []string{id})
			if errors.Is(err, errDelivery) != fail || posts != 1 || *summaries != 1 {
				t.Fatalf("投稿失敗の伝播が不正: %v posts=%d summaries=%d", err, posts, *summaries)
			}
			if fail {
				if checkpoints != 0 || r.state.done([]string{id}) {
					t.Fatal("投稿失敗を成功として記録しました")
				}
			} else {
				if err != nil || checkpoints != 1 {
					t.Fatal("投稿成功後の保存に失敗", err)
				}
				if err := r.process(context.Background(), "source", mode, srv.URL+"/article", "記事", "", "", []string{id}); err != nil || *summaries != 1 || posts != 1 {
					t.Fatal("投稿済み記事のAPIを再度呼びました", err)
				}
			}
		})
	}
}
