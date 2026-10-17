package main

import (
	"golang.org/x/net/html"
	"strings"
)

func parseHTMLForTest(s string) (*html.Node, error) { return html.Parse(strings.NewReader(s)) }
