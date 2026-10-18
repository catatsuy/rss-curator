package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	readability "github.com/mackee/go-readability"
	"golang.org/x/net/html"
)

type Article struct {
	Title, URL, Markdown, HTML string
	PDF, Paper                 bool
	httpStatus                 int
}
type Link struct{ Title, URL, Context string }

const defaultArticleUserAgent = "rss-curator (+https://github.com/catatsuy/rss-curator)"

func normalize(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u == nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", fmt.Errorf("記事URLが不正です")
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	q := u.Query()
	for k := range q {
		l := strings.ToLower(k)
		if strings.HasPrefix(l, "utm_") || l == "fbclid" || l == "gclid" || l == "mc_cid" || l == "mc_eid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}
func textContent(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			b.WriteByte(' ')
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}
func resolve(base, ref string) string {
	b, e := url.Parse(base)
	if e != nil {
		return ""
	}
	r, e := url.Parse(ref)
	if e != nil {
		return ""
	}
	s, _ := normalize(b.ResolveReference(r).String())
	return s
}
func canonical(doc *html.Node, base string) string {
	b, _ := url.Parse(base)
	result := base
	var candidates []string
	ogURL := ""
	walk(doc, func(n *html.Node) {
		if n.Data == "meta" && attr(n, "property") == "og:url" {
			ogURL = resolve(base, attr(n, "content"))
		}
		if n.Data != "link" {
			return
		}
		for _, rel := range strings.Fields(strings.ToLower(attr(n, "rel"))) {
			if rel != "canonical" {
				continue
			}
			s := resolve(base, attr(n, "href"))
			u, e := url.Parse(s)
			if e == nil && u.Host == b.Host && u.Path != "/" && s != "" && attr(n, "href") != "" {
				candidates = append(candidates, s)
			}
		}
	})
	// 複数の異なる宣言やOGメタデータとの矛盾があれば最終URLを使う。
	if len(candidates) > 0 {
		result = candidates[0]
		for _, candidate := range candidates {
			if candidate != result {
				return base
			}
		}
		if ogURL != "" && ogURL != result {
			return base
		}
	}
	return result
}
func isPDF(u, mime string) bool {
	p, e := url.Parse(u)
	if e != nil {
		return strings.EqualFold(strings.TrimSpace(strings.Split(mime, ";")[0]), "application/pdf")
	}
	return strings.EqualFold(path.Ext(p.Path), ".pdf") || strings.EqualFold(strings.TrimSpace(strings.Split(mime, ";")[0]), "application/pdf")
}
func fetchArticle(ctx context.Context, c *http.Client, raw, title, mime string) (Article, error) {
	a := Article{Title: title}
	u, e := normalize(raw)
	if e != nil {
		return a, e
	}
	a.URL = u
	if isPDF(u, mime) {
		a.PDF = true
		return a, nil
	}
	userAgent := strings.TrimSpace(os.Getenv("ARTICLE_USER_AGENT"))
	if userAgent == "" {
		userAgent = defaultArticleUserAgent
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	req.Header.Set("User-Agent", userAgent)
	r, e := c.Do(req)
	if e == nil {
		r.Body.Close()
		if r.StatusCode == http.StatusTooManyRequests {
			return a, newArticleHTTPError(r)
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 && isPDF(r.Request.URL.String(), r.Header.Get("Content-Type")) {
			a.URL, _ = normalize(r.Request.URL.String())
			a.PDF = true
			a.httpStatus = r.StatusCode
			return a, nil
		}
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", userAgent)
	r, e = c.Do(req)
	if e != nil {
		return a, fmt.Errorf("記事の取得に失敗しました")
	}
	return readArticleResponse(a, r)
}

// 通常HTTPと代替取得のレスポンスを、同じ本文抽出処理へ渡す。
func readArticleResponse(a Article, r *http.Response) (Article, error) {
	defer r.Body.Close()
	a.httpStatus = r.StatusCode
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return a, newArticleHTTPError(r)
	}
	a.URL, _ = normalize(r.Request.URL.String())
	if isPDF(a.URL, r.Header.Get("Content-Type")) {
		a.PDF = true
		return a, nil
	}
	const limit = 20 << 20
	b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if e != nil || len(b) > limit {
		return a, fmt.Errorf("記事の読み込みに失敗、または20MiBを超えています")
	}
	doc, e := html.Parse(strings.NewReader(string(b)))
	if e != nil {
		return a, fmt.Errorf("HTMLの解析に失敗しました")
	}
	finalURL := a.URL
	a.URL = canonical(doc, finalURL)
	walk(doc, func(n *html.Node) {
		if n.Data == "meta" {
			name := attr(n, "name")
			if name == "citation_title" && attr(n, "content") != "" {
				a.Paper = true
			}
			if name == "citation_doi" && attr(n, "content") != "" {
				a.Paper = true
			}
		}
		if n.Data == "a" || n.Data == "link" {
			v := attr(n, "href")
			if v != "" {
				for i := range n.Attr {
					if n.Attr[i].Key == "href" {
						n.Attr[i].Val = resolve(finalURL, v)
					}
				}
			}
		}
		if n.Data == "img" {
			for i := range n.Attr {
				if n.Attr[i].Key == "src" {
					n.Attr[i].Val = resolve(finalURL, n.Attr[i].Val)
				}
			}
		}
	})
	if a.Paper {
		return a, nil
	}
	var buf strings.Builder
	_ = html.Render(&buf, doc)
	extracted, e := readability.Extract(buf.String(), readability.DefaultOptions())
	if e != nil || extracted.Root == nil {
		return a, fmt.Errorf("本文を抽出できませんでした")
	}
	a.HTML = readability.ToHTML(extracted.Root)
	a.Markdown = readability.ToMarkdown(extracted.Root)
	if strings.TrimSpace(a.Markdown) == "" {
		return a, fmt.Errorf("抽出した本文が空です")
	}
	if a.Title == "" {
		a.Title = extracted.Title
	}
	return a, nil
}
func digestLinks(a Article) []Link {
	doc, e := html.Parse(strings.NewReader(a.HTML))
	if e != nil {
		return nil
	}
	base, _ := url.Parse(a.URL)
	seen := map[string]bool{}
	var links []Link
	walk(doc, func(n *html.Node) {
		if n.Data != "a" {
			return
		}
		for p := n.Parent; p != nil; p = p.Parent {
			if p.Data == "nav" || p.Data == "footer" || p.Data == "aside" {
				return
			}
			s := strings.ToLower(attr(p, "class") + " " + attr(p, "id"))
			for _, bad := range []string{"sponsor", "advert", "subscribe", "social", "share"} {
				if strings.Contains(s, bad) {
					return
				}
			}
		}
		u := resolve(a.URL, attr(n, "href"))
		p, e := url.Parse(u)
		if e != nil || u == "" || p.Hostname() == base.Hostname() || seen[u] {
			return
		}
		host := strings.ToLower(p.Hostname())
		for _, bad := range []string{"twitter.com", "x.com", "facebook.com", "linkedin.com", "mailto"} {
			if host == bad || strings.HasSuffix(host, "."+bad) {
				return
			}
		}
		t := textContent(n)
		lower := strings.ToLower(t + " " + p.Path)
		for _, bad := range []string{"unsubscribe", "subscribe", "sign up", "share/", "privacy-policy"} {
			if strings.Contains(lower, bad) {
				return
			}
		}
		parent := n.Parent
		for parent != nil && parent.Data != "p" && parent.Data != "li" && parent.Data != "div" {
			parent = parent.Parent
		}
		if parent == nil {
			return
		}
		intro := textContent(parent)
		if len([]rune(intro)) < 30 {
			return
		}
		seen[u] = true
		links = append(links, Link{t, u, intro})
	})
	return links
}
