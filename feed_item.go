package main

import (
	"context"
	"strings"

	"github.com/mmcdole/gofeed"
)

func (r *runner) processFeedItem(ctx context.Context, f Feed, item *gofeed.Item, raw, mime string, keys []string) error {
	r.skipHTTP = f.SkipHTTP
	if f.FeedContent && strings.TrimSpace(item.Content) != "" && mime == "" {
		return r.processWithContent(ctx, f.Name, f.Mode, raw, item.Title, mime, "", keys, item.Content)
	}
	return r.process(ctx, f.Name, f.Mode, raw, item.Title, mime, "", keys)
}
