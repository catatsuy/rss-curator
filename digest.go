package main

import "time"

// DigestState は紹介先のハッシュだけを保存し、URLや紹介文は公開しない。
type DigestState struct {
	Items    []string  `json:"items"`
	LastSeen time.Time `json:"last_seen"`
}

func (r *runner) rememberDigest(marker string, links []Link) {
	if r.state.Digests == nil {
		r.state.Digests = map[string]DigestState{}
	}
	items := make([]string, 0, len(links))
	for _, link := range links {
		items = append(items, hash(link.URL))
	}
	r.state.Digests[marker] = DigestState{Items: items, LastSeen: r.fetchTime()}
}

// 本体の投稿と紹介先の処理が完了したら、以後はHTTPもCrawlbaseも使用しない。
func (r *runner) digestNeedsFetch(marker string, keys []string) bool {
	if !r.state.done(keys) {
		return true
	}
	if _, complete := r.state.Processed[marker]; complete {
		return false
	}
	d, known := r.state.Digests[marker]
	if !known {
		// 旧状態は一度だけ紹介先の一覧を発見する必要がある。
		return true
	}
	d.LastSeen = r.fetchTime()
	r.state.Digests[marker] = d
	pending, eligible := false, false
	maxFailures := r.maxFetchFailures
	if maxFailures == 0 {
		maxFailures = defaultFetchFailures
	}
	for _, id := range d.Items {
		if r.state.done([]string{id}) {
			continue
		}
		f, tracked := r.state.Fetch[id]
		if tracked {
			f.LastSeen = r.fetchTime()
			r.state.Fetch[id] = f
		}
		if f.Stopped || f.Failures >= maxFailures {
			f.Stopped = true
			if f.StopReason == "" {
				f.StopReason = "fetch_failure_limit"
			}
			f.NextAttempt = time.Time{}
			r.state.Fetch[id] = f
			r.state.Processed[id] = r.fetchTime()
			continue
		}
		pending = true
		if r.claimed[id] || r.fetchAttempted[id] || r.fetchTime().Before(f.NextAttempt) {
			continue
		}
		if f.Method == fetchCrawlbase {
			if r.crawlbase == nil || r.crawlbase.token == "" || r.crawlbase.disabled || r.crawlbase.used >= r.crawlbase.limit || r.fetchTime().Before(r.state.CrawlbaseRetryAfter) {
				continue
			}
		}
		eligible = true
	}
	if !pending {
		r.state.Processed[marker] = r.fetchTime()
		delete(r.state.Digests, marker)
	}
	return eligible
}
