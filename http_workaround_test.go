package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestHTTPWorkaroundConfig(t *testing.T) {
	for _, flag := range []string{"", `,"http_workaround":false`, `,"http_workaround":true`} {
		path := filepath.Join(t.TempDir(), "feeds.json")
		mustWrite(t, path, `[{"name":"Source","url":"https://example.com/feed","mode":"select"`+flag+`}]`)
		feeds, err := readFeeds(path)
		if err != nil || feeds[0].HTTPWorkaround != (flag == `,"http_workaround":true`) {
			t.Fatal("回避フラグが正しく読み込まれません", feeds, err)
		}
	}
	feeds, err := readFeeds("feeds.json")
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, feed := range feeds {
		if feed.HTTPWorkaround {
			enabled++
			if feed.Name != "OpenAI" {
				t.Fatal("他のフィードにも回避処理が有効です")
			}
		}
	}
	if enabled != 1 {
		t.Fatal("OpenAIだけで有効になっていません")
	}
}

func TestHTTPWorkaroundPreservesNormalClient(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}
	jar, _ := cookiejar.New(nil)
	checks := 0
	client := &http.Client{Transport: base, Timeout: 17 * time.Second, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { checks++; return http.ErrUseLastResponse }}
	modified, err := newHTTPWorkaroundClient(client, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modified.CloseIdleConnections)
	wrapped := modified.Transport.(*httpWorkaroundTransport)
	workaround := wrapped.workaround
	if modified.Timeout != client.Timeout || modified.Jar != jar || wrapped.normal != base {
		t.Fatal("通常クライアントの設定を失いました")
	}
	if err := modified.CheckRedirect(nil, nil); err != http.ErrUseLastResponse || checks != 1 {
		t.Fatal("転送ポリシーを失いました")
	}
	if workaround.Protocols.HTTP2() || !workaround.Protocols.HTTP1() || workaround.ForceAttemptHTTP2 || !reflect.DeepEqual(workaround.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatal("HTTP/1.1に限定されていません")
	}
	if !reflect.DeepEqual(workaround.TLSClientConfig.CurvePreferences, []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}) {
		t.Fatal("回避設定の鍵交換曲線が不正です")
	}
	if workaround.TLSClientConfig.InsecureSkipVerify || workaround.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatal("証明書検証・TLS下限を変更しました")
	}
	if base.TLSClientConfig.CurvePreferences != nil || base.Protocols != nil || !reflect.DeepEqual(base.TLSClientConfig.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatal("通常Transportを書き換えました")
	}
}

func TestHTTPWorkaroundDomainRouting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/start" {
			http.Redirect(w, req, "http://other.example/final", http.StatusFound)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	normalCalls, workaroundCalls := 0, 0
	dialer := &net.Dialer{}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DisableKeepAlives = true
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		normalCalls++
		return dial(ctx, network, addr)
	}
	defer base.CloseIdleConnections()
	client, err := newHTTPWorkaroundClient(&http.Client{Transport: base}, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	wrapped := client.Transport.(*httpWorkaroundTransport)
	wrapped.workaround.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		workaroundCalls++
		return dial(ctx, network, addr)
	}
	for _, raw := range []string{"http://example.com/start", "http://sub.example.com/final", "http://example.com.evil.test/final", "http://api.crawlbase.com/final"} {
		resp, err := client.Get(raw)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if workaroundCalls != 2 || normalCalls != 3 {
		t.Fatal("対象外ドメインに回避設定を適用しました", workaroundCalls, normalCalls)
	}
	// フラグなしの通常クライアントは同じ対象ドメインでも通常経路を使用する。
	resp, err := (&http.Client{Transport: base}).Get("http://example.com/final")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if normalCalls != 4 || workaroundCalls != 2 {
		t.Fatal("通常経路が変化しました")
	}
}
