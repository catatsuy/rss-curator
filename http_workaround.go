package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
)

// 明示的なフラグでのみ使用する。通常のHTTP/TLS設定には混ぜない。
func newHTTPWorkaroundClient(client *http.Client, host string) (*http.Client, error) {
	normal := client.Transport
	if normal == nil {
		normal = http.DefaultTransport
	}
	base, ok := normal.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("HTTP回避設定には標準HTTP Transportが必要です")
	}
	workaround := base.Clone()
	workaround.Protocols = new(http.Protocols)
	workaround.Protocols.SetHTTP1(true)
	workaround.ForceAttemptHTTP2 = false
	workaround.TLSNextProto = nil
	if workaround.TLSClientConfig == nil {
		workaround.TLSClientConfig = &tls.Config{}
	}
	workaround.TLSClientConfig.NextProtos = []string{"http/1.1"}
	workaround.TLSClientConfig.CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}
	c := *client
	c.Transport = &httpWorkaroundTransport{normal: normal, workaround: workaround, host: strings.ToLower(host)}
	return &c, nil
}

type httpWorkaroundTransport struct {
	host       string
	normal     http.RoundTripper
	workaround *http.Transport
}

func (t *httpWorkaroundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if host == t.host || strings.HasSuffix(host, "."+t.host) {
		return t.workaround.RoundTrip(req)
	}
	return t.normal.RoundTrip(req)
}

func (t *httpWorkaroundTransport) CloseIdleConnections() {
	t.workaround.CloseIdleConnections()
}
