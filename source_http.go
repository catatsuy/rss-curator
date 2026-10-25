package main

import (
	"net/http"
	"os"
	"strings"
)

const browserSourceUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

func isOpenAIHost(host string) bool {
	host = strings.ToLower(host)
	return host == "openai.com" || strings.HasSuffix(host, ".openai.com")
}

// RSSと本文で共用する。元のTransport・タイムアウト・Cookie・リダイレクト制御を維持する。
func doSourceRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c := *client
	c.Transport = &sourceUserAgentTransport{base: transport, baseUA: req.Header.Get("User-Agent")}
	return c.Do(req)
}

type sourceUserAgentTransport struct {
	base   http.RoundTripper
	baseUA string
}

func (t *sourceUserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	ua := t.baseUA
	host := strings.ToLower(request.URL.Hostname())
	if isOpenAIHost(host) || host == "medium.com" || strings.HasSuffix(host, ".medium.com") {
		ua = strings.TrimSpace(os.Getenv("ARTICLE_USER_AGENT"))
		if ua == "" {
			ua = browserSourceUserAgent
		}
	}
	if ua == "" {
		request.Header.Del("User-Agent")
	} else {
		request.Header.Set("User-Agent", ua)
	}
	return t.base.RoundTrip(request)
}
