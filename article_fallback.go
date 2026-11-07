package main

import (
	"strings"
	"unicode/utf8"

	readability "github.com/mackee/go-readability"
	"golang.org/x/net/html"
)

// Readabilityが本文を選べない場合だけ、明示された本文領域を利用する。
// body全体へは広げず、短いエラー画面やリンク集を本文として採用しない。
func structuredArticleContent(doc *html.Node) (string, string) {
	var best *html.Node
	bestLength := 0
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		candidate := n.Data == "main" || n.Data == "article" || attr(n, "role") == "main"
		for _, class := range strings.Fields(attr(n, "class")) {
			candidate = candidate || class == "field--name-body"
		}
		if !candidate {
			return
		}
		clean := cloneArticleContent(n)
		if clean == nil {
			return
		}
		length, linked, prose := 0, 0, 0
		walk(clean, func(c *html.Node) {
			if c.Type == html.TextNode {
				size := utf8.RuneCountInString(strings.TrimSpace(c.Data))
				length += size
				for p := c.Parent; p != nil; p = p.Parent {
					if p.Data == "a" {
						linked += size
						break
					}
				}
			}
			if c.Data == "p" {
				prose += utf8.RuneCountInString(textContent(c))
			}
		})
		if prose >= 500 && linked*2 <= length && length > bestLength {
			best, bestLength = clean, length
		}
	})
	if best == nil {
		return "", ""
	}
	var buf strings.Builder
	_ = html.Render(&buf, best)
	parsed, err := readability.ParseHTML(buf.String(), "")
	if err != nil {
		return "", ""
	}
	return readability.ToHTML(parsed.Body), readability.ToMarkdown(parsed.Body)
}

func cloneArticleContent(n *html.Node) *html.Node {
	if n.Type == html.ElementNode {
		switch n.Data {
		case "nav", "header", "footer", "aside", "script", "style", "noscript", "form", "button", "iframe":
			return nil
		}
		if attr(n, "aria-hidden") == "true" {
			return nil
		}
		for _, a := range n.Attr {
			if a.Key == "hidden" {
				return nil
			}
		}
		classes := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
		for _, bad := range []string{"sidebar", "newsletter", "social", "share-links", "related", "advert", "cookie-banner"} {
			if strings.Contains(classes, bad) {
				return nil
			}
		}
	}
	clone := &html.Node{Type: n.Type, DataAtom: n.DataAtom, Data: n.Data, Namespace: n.Namespace, Attr: append([]html.Attribute(nil), n.Attr...)}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if child := cloneArticleContent(c); child != nil {
			clone.AppendChild(child)
		}
	}
	return clone
}
