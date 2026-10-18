package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArticleUserAgent(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, tc := range []struct{ config, want string }{
			{"", "rss-curator (+https://github.com/catatsuy/rss-curator)"},
			{"   ", "rss-curator (+https://github.com/catatsuy/rss-curator)"},
			{"CustomReader/1.0", "CustomReader/1.0"},
			{"  CustomReader/1.0  ", "CustomReader/1.0"},
		} {
			t.Run(fmt.Sprintf("http2=%v/ua=%s", http2, tc.config), func(t *testing.T) {
				t.Setenv("ARTICLE_USER_AGENT", tc.config)
				methods := map[string]int{}
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					methods[req.Method]++
					if req.Header.Get("User-Agent") != tc.want {
						t.Errorf("UA: got %q, want %q", req.Header.Get("User-Agent"), tc.want)
					}
					if http2 && req.ProtoMajor != 2 {
						t.Error("HTTP/2で検証していません")
					}
					if req.URL.Path == "/start" {
						http.Redirect(w, req, "/article", http.StatusFound)
						return
					}
					fmt.Fprint(w, fixtureHTML("記事", ""))
				}))
				server.EnableHTTP2 = http2
				if http2 {
					server.StartTLS()
				} else {
					server.Start()
				}
				defer server.Close()
				a, err := fetchArticle(context.Background(), server.Client(), server.URL+"/start", "記事", "")
				if err != nil || !strings.Contains(a.Markdown, "12ms") {
					t.Fatal(a, err)
				}
				if methods[http.MethodHead] != 2 || methods[http.MethodGet] != 2 {
					t.Fatal("HEAD/GETとリダイレクト先を検証していません", methods)
				}
			})
		}
	}
}
