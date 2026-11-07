package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	fetchHTTP            = "http"
	fetchJA4             = "ja4"
	fetchCrawlbase       = "crawlbase"
	crawlbaseEndpoint    = "https://api.crawlbase.com/"
	maxCrawlbaseFailures = 3
	fetchRetention       = 30 * 24 * time.Hour
	rateLimitWait        = time.Hour
	defaultFetchFailures = 3
)

// FetchState はURLや認証情報を保存せず、既存の記事ハッシュに取得状態を対応させる。
type FetchState struct {
	Method               string    `json:"method"`
	LastFailure          string    `json:"last_failure,omitempty"`
	Attempts             int       `json:"attempts"`
	Failures             int       `json:"failures"`
	CrawlbaseAttempts    int       `json:"crawlbase_attempts,omitempty"`
	CrawlbaseFailures    int       `json:"crawlbase_failures,omitempty"`
	LastAttempt          time.Time `json:"last_attempt,omitzero"`
	NextAttempt          time.Time `json:"next_attempt,omitzero"`
	LastSeen             time.Time `json:"last_seen"`
	FailureNoticePending bool      `json:"failure_notice_pending,omitempty"`
	Stopped              bool      `json:"stopped,omitempty"`
	StopReason           string    `json:"stop_reason,omitempty"`
}

func fetchFailureLimit() (int, error) {
	limit, err := strconv.Atoi(env("ARTICLE_MAX_FETCH_FAILURES", "3"))
	if err != nil || limit < 1 {
		return 0, fmt.Errorf("ARTICLE_MAX_FETCH_FAILURESは1以上の整数にしてください")
	}
	return limit, nil
}

// 本文取得に適さない動画・求人・購読等への通信を、代替取得も含めて避ける。
func excludedArticleURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, domain := range []string{"youtube.com", "youtu.be", "youtube-nocookie.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return "excluded_video"
		}
	}
	for _, domain := range []string{"workable.com", "greenhouse.io", "lever.co"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return "excluded_job"
		}
	}
	if strings.HasPrefix(host, "jobs.") || strings.HasPrefix(host, "careers.") {
		return "excluded_job"
	}
	for _, part := range strings.Split(strings.ToLower(u.Path), "/") {
		switch part {
		case "jobs", "careers":
			return "excluded_job"
		case "donate", "donation", "donations", "subscribe", "unsubscribe":
			return "excluded_subscription"
		}
	}
	return ""
}

type articleHTTPError struct {
	Status     int
	RetryAfter string
	Host       string
}

func (e *articleHTTPError) Error() string {
	return fmt.Sprintf("記事のHTTPステータス: %d", e.Status)
}
func newArticleHTTPError(r *http.Response) *articleHTTPError {
	return &articleHTTPError{Status: r.StatusCode, RetryAfter: r.Header.Get("Retry-After"), Host: r.Request.URL.Hostname()}
}

var errFetchDeferred = errors.New("記事の取得を次回に繰り越します")

type crawlbaseClient struct {
	client      *http.Client
	token       string
	limit, used int
	disabled    bool
}

func newCrawlbaseClient(c *http.Client) (*crawlbaseClient, error) {
	limit, err := strconv.Atoi(env("CRAWLBASE_MAX_REQUESTS", "5"))
	if err != nil || limit < 0 {
		return nil, fmt.Errorf("CRAWLBASE_MAX_REQUESTSは0以上の整数にしてください")
	}
	// クエリにトークンを含むため、APIのリダイレクトへは追従しない。
	client := *c
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	return &crawlbaseClient{client: &client, token: strings.TrimSpace(os.Getenv("CRAWLBASE_TOKEN")), limit: limit}, nil
}

type crawlResult struct {
	HTTPStatus, CBStatus, OriginalStatus   int
	RetryAfter                             string
	Reason                                 string
	Permanent, RateLimited, ServiceLimited bool
}

func (c *crawlbaseClient) fetch(ctx context.Context, target, title string) (Article, crawlResult, error) {
	resp, result, err := c.fetchResponse(ctx, target, map[string]bool{})
	a := Article{Title: title, URL: target}
	if err != nil {
		return a, result, err
	}
	a, err = readArticleResponse(a, resp)
	if errors.Is(err, errBotChallenge) {
		result.Reason = "bot_challenge"
		result.Permanent = true
	} else if errors.Is(err, errNoArticleContent) {
		result.Reason = "no_article_content"
	} else if err != nil {
		result.Reason = "extraction_failed"
	}
	return a, result, err
}

func (c *crawlbaseClient) fetchResponse(ctx context.Context, target string, visited map[string]bool) (*http.Response, crawlResult, error) {
	result := crawlResult{}
	if visited[target] || len(visited) >= 10 {
		result.Reason = "crawlbase_redirect_loop"
		return nil, result, fmt.Errorf("Crawlbaseの転送が循環、または10回を超えました")
	}
	if len(visited) > 0 && c.used >= c.limit {
		result.Reason = "crawlbase_limit"
		return nil, result, errFetchDeferred
	}
	visited[target] = true
	q := url.Values{"token": {c.token}, "url": {target}, "get_headers": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, crawlbaseEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		result.Reason = "crawlbase_request"
		return nil, result, fmt.Errorf("Crawlbaseのリクエストを作成できません")
	}
	c.used++
	resp, err := c.client.Do(req)
	if err != nil {
		result.Reason = "crawlbase_transport"
		return nil, result, fmt.Errorf("Crawlbaseへの接続に失敗しました")
	}
	result.HTTPStatus = resp.StatusCode
	result.CBStatus, _ = strconv.Atoi(resp.Header.Get("cb_status"))
	result.OriginalStatus, _ = strconv.Atoi(resp.Header.Get("original_status"))
	result.RetryAfter = resp.Header.Get("original_header_retry-after")
	if result.RetryAfter == "" {
		result.RetryAfter = resp.Header.Get("Retry-After")
	}
	// 転送レスポンスの本文を記事として扱わず、転送先を明示的に再取得する。
	if resp.StatusCode == http.StatusOK && result.CBStatus == 200 && isArticleRedirect(result.OriginalStatus) {
		next := ""
		if location := resp.Header.Get("original_header_location"); location != "" {
			next = resolve(target, location)
		} else {
			next, _ = normalize(resp.Header.Get("url"))
		}
		resp.Body.Close()
		u, err := url.Parse(next)
		if err != nil || next == "" || strings.EqualFold(u.Hostname(), "api.crawlbase.com") || excludedArticleURL(next) != "" {
			result.Reason = "crawlbase_redirect_invalid"
			return nil, result, fmt.Errorf("Crawlbaseの転送先URLが不正です")
		}
		slog.Info("Crawlbaseの記事転送", "url", safeArticleURL(target), "next_url", safeArticleURL(next), "original_status", result.OriginalStatus, "crawlbase_requests", c.used)
		return c.fetchResponse(ctx, next, visited)
	}
	// 520は、対象サイトの取得失敗を伝えるAPIのHTTPステータスにも使われる。
	if resp.StatusCode == http.StatusOK || resp.StatusCode == 520 {
		switch {
		case result.OriginalStatus == 429 || result.CBStatus == 429 || result.CBStatus == 706:
			result.Reason = "target_rate_limited"
			result.Permanent = true
			result.RateLimited = true
		case result.OriginalStatus == 403 || result.CBStatus == 705:
			result.Reason = "target_forbidden"
			result.Permanent = true
		case result.OriginalStatus == 401 || result.OriginalStatus == 404 || result.OriginalStatus == 410 || result.OriginalStatus == 451:
			result.Reason = "target_permanent"
			result.Permanent = true
		case result.CBStatus == 403 || result.CBStatus == 404 || result.CBStatus == 410 || result.CBStatus == 451 || result.CBStatus == 503 || result.CBStatus == 525:
			result.Reason = "target_blocked"
			result.Permanent = true
		case result.CBStatus == 207:
			result.Reason = "target_empty"
			result.Permanent = true
		case result.OriginalStatus >= 500:
			result.Reason = "target_server_error"
		case result.CBStatus == 200 && result.OriginalStatus >= 200 && result.OriginalStatus < 300 && resp.StatusCode == 200:
			result.Reason = "success"
		case result.CBStatus >= 400 && result.CBStatus < 500:
			result.Reason = "crawlbase_request"
			result.Permanent = true
		default:
			result.Reason = "crawlbase_upstream_error"
		}
	} else {
		result.Reason = "crawlbase_service_error"
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			result.Reason = "crawlbase_configuration"
			result.Permanent = true
			if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 {
				c.disabled = true
			}
		}
		if resp.StatusCode == 429 {
			result.Reason = "crawlbase_rate_limited"
			result.ServiceLimited = true
			result.RetryAfter = resp.Header.Get("Retry-After")
		}
	}
	if result.Reason != "success" {
		resp.Body.Close()
		return nil, result, fmt.Errorf("Crawlbaseの取得に失敗しました（HTTP %d、cb_status %d、original_status %d）", result.HTTPStatus, result.CBStatus, result.OriginalStatus)
	}
	final := target
	if candidate, e := normalize(resp.Header.Get("url")); e == nil {
		// APIエンドポイントを記事URLとして扱わない。
		parsed, _ := url.Parse(candidate)
		if !strings.EqualFold(parsed.Hostname(), "api.crawlbase.com") {
			final = candidate
		}
	}
	parsed, _ := url.Parse(final)
	resp.Request = &http.Request{URL: parsed}
	resp.StatusCode = result.OriginalStatus
	if typ := resp.Header.Get("original_header_content-type"); typ != "" {
		resp.Header.Set("Content-Type", typ)
	}
	return resp, result, nil
}

func isArticleRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

func (r *runner) fetchTime() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}
func (s *State) pruneFetch(now time.Time) {
	if s.Fetch == nil {
		s.Fetch = map[string]FetchState{}
	}
	if s.HostRetryAfter == nil {
		s.HostRetryAfter = map[string]time.Time{}
	}
	for id, f := range s.Fetch {
		// 投稿済みdigest本体にも、紹介先の再発見のための取得予約があり得る。
		if now.Sub(f.LastSeen) > fetchRetention || (!f.Stopped && f.LastFailure == "" && s.done([]string{id})) {
			delete(s.Fetch, id)
		}
	}
	for host, until := range s.HostRetryAfter {
		if !now.Before(until) {
			delete(s.HostRetryAfter, host)
		}
	}
	if !now.Before(s.CrawlbaseRetryAfter) {
		s.CrawlbaseRetryAfter = time.Time{}
	}
	for marker, d := range s.Digests {
		if now.Sub(d.LastSeen) > fetchRetention {
			delete(s.Digests, marker)
		}
	}
}

// Retry-Afterがない/不正/短すぎる場合も、429の後は最低1時間待つ。
func retryAfter(value string, now time.Time) time.Time {
	until := now.Add(rateLimitWait)
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		if candidate := now.Add(time.Duration(seconds) * time.Second); candidate.After(until) {
			until = candidate
		}
	} else if candidate, err := http.ParseTime(value); err == nil && candidate.After(until) {
		until = candidate
	}
	return until
}

func safeArticleURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid_url"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// 1回の試行内で通常HTTP → JA4 → Crawlbaseの順に取得する。
func (r *runner) fetchArticle(ctx context.Context, raw, title, mime string) (Article, error) {
	return r.fetchArticleWithPolicy(ctx, raw, title, mime, false)
}

// 任意の日本語版の不在は、既読や取得断念通知にしない。
func (r *runner) fetchArticleWithPolicy(ctx context.Context, raw, title, mime string, optional bool) (Article, error) {
	notify := func(u string) error {
		if optional {
			return nil
		}
		return r.notifyFetchFailure(u)
	}
	markStopped := func(id string, now time.Time) {
		if !optional {
			r.state.Processed[id] = now
		}
	}
	u, err := normalize(raw)
	if err != nil {
		return Article{}, err
	}
	now := r.fetchTime()
	if r.state.Fetch == nil {
		r.state.Fetch = map[string]FetchState{}
	}
	if r.state.HostRetryAfter == nil {
		r.state.HostRetryAfter = map[string]time.Time{}
	}
	if r.fetchAttempted == nil {
		r.fetchAttempted = map[string]bool{}
	}
	id := hash(u)
	parsed, _ := url.Parse(u)
	host := hash(parsed.Hostname())
	f := r.state.Fetch[id]
	if f.Method == "" || f.Method == fetchHTTP {
		f.Method = r.initialFetchMethod()
	}
	f.LastSeen = now
	result := crawlResult{}
	method := f.Method
	maxFailures := r.maxFetchFailures
	if maxFailures == 0 {
		maxFailures = defaultFetchFailures
	}
	outcome := "deferred"
	defer func() {
		calls := 0
		if r.crawlbase != nil {
			calls = r.crawlbase.used
		}
		slog.Info("記事の取得", "url", safeArticleURL(u), "method", method, "result", outcome, "http_status", result.HTTPStatus, "cb_status", result.CBStatus, "original_status", result.OriginalStatus, "reason", result.Reason, "stop_reason", f.StopReason, "attempts", f.Attempts, "failures", f.Failures, "max_failures", maxFailures, "next_method", f.Method, "retry_scheduled", outcome != "success" && !f.Stopped, "retry_at", f.NextAttempt, "crawlbase_requests", calls)
	}()
	deferFetch := func(reason string) (Article, error) {
		result.Reason = reason
		r.state.Fetch[id] = f
		return Article{}, errFetchDeferred
	}
	if f.Stopped {
		if e := notify(u); e != nil {
			return Article{}, e
		}
		f = r.state.Fetch[id]
		return deferFetch("stopped")
	}
	reason := excludedArticleURL(u)
	if reason != "" || f.Failures >= maxFailures {
		outcome = "skipped"
		if reason == "" {
			reason = "fetch_failure_limit"
			outcome = "stopped"
		}
		result.Reason = reason
		f.FailureNoticePending = !optional && reason == "fetch_failure_limit"
		f.Stopped = true
		f.StopReason = reason
		f.NextAttempt = time.Time{}
		r.state.Fetch[id] = f
		markStopped(id, now)
		if e := r.save(); e != nil {
			return Article{}, fmt.Errorf("%w: %v", errDelivery, e)
		}
		if e := notify(u); e != nil {
			return Article{}, e
		}
		return Article{}, errFetchDeferred
	}
	if r.fetchAttempted[id] {
		return deferFetch("already_attempted")
	}
	if until := r.state.HostRetryAfter[host]; now.Before(until) {
		if until.After(f.NextAttempt) {
			f.NextAttempt = until
		}
		return deferFetch("host_cooldown")
	}
	if now.Before(f.NextAttempt) {
		return deferFetch("retry_wait")
	}
	if method == fetchCrawlbase {
		if reason := r.crawlbaseUnavailable(now); reason != "" {
			return deferFetch(reason)
		}
	}
	r.fetchAttempted[id] = true
	f.Attempts++
	f.LastAttempt = now
	f.NextAttempt = time.Time{}
	var a Article
	for {
		method = f.Method
		result = crawlResult{}
		if method == fetchCrawlbase {
			if reason := r.crawlbaseUnavailable(now); reason != "" {
				if r.state.CrawlbaseRetryAfter.After(f.NextAttempt) {
					f.NextAttempt = r.state.CrawlbaseRetryAfter
				}
				if saveErr := r.saveFetch(id, f); saveErr != nil {
					return Article{}, saveErr
				}
				return deferFetch(reason)
			}
			f.CrawlbaseAttempts++
			a, result, err = r.crawlbase.fetch(ctx, u, title)
			if errors.Is(err, errFetchDeferred) {
				if saveErr := r.saveFetch(id, f); saveErr != nil {
					return Article{}, saveErr
				}
				return deferFetch(result.Reason)
			}
			if errors.Is(err, errNoArticleContent) {
				break
			}
			if err != nil {
				f.CrawlbaseFailures++
				f.LastFailure = result.Reason
				if result.Permanent || f.CrawlbaseFailures >= maxCrawlbaseFailures {
					f.Stopped = true
					markStopped(id, now)
				} else {
					f.NextAttempt = now.Add(rateLimitWait)
				}
				if result.RateLimited || result.ServiceLimited {
					until := retryAfter(result.RetryAfter, now)
					if result.ServiceLimited {
						r.state.CrawlbaseRetryAfter = until
					} else {
						r.state.HostRetryAfter[host] = until
					}
					if !f.Stopped && until.After(f.NextAttempt) {
						f.NextAttempt = until
					}
				}
			} else {
				f.LastFailure = ""
				f.CrawlbaseFailures = 0
			}
			break
		}
		client := r.client
		if method == fetchJA4 {
			client, err = r.browserClient()
		}
		if method != fetchJA4 || err == nil {
			a, err = fetchArticle(ctx, client, u, title, mime)
		}
		result.HTTPStatus = a.httpStatus
		if err == nil {
			f.LastFailure = ""
			result.Reason = "success"
			break
		}
		if errors.Is(err, errNoArticleContent) {
			break
		}
		f.LastFailure = "http_transport"
		var status *articleHTTPError
		if errors.As(err, &status) {
			result.HTTPStatus = status.Status
			f.LastFailure = fmt.Sprintf("http_%d", status.Status)
			switch status.Status {
			case 429:
				f.Method = fetchJA4
				if method == fetchJA4 {
					f.Method = fetchCrawlbase
				}
				f.NextAttempt = retryAfter(status.RetryAfter, now)
				r.state.HostRetryAfter[host] = f.NextAttempt
				r.state.HostRetryAfter[hash(status.Host)] = f.NextAttempt
			case 401, 404, 410, 451:
				f.Stopped = true
				markStopped(id, now)
			}
		} else if errors.Is(err, errBotChallenge) {
			f.LastFailure = "bot_challenge"
		}
		result.Reason = f.LastFailure
		if f.Stopped || !f.NextAttempt.IsZero() || ctx.Err() != nil {
			break
		}
		next := fetchJA4
		if method == fetchJA4 {
			next = fetchCrawlbase
		}
		slog.Info("記事取得のフォールバック", "url", safeArticleURL(u), "method", method, "next_method", next, "http_status", result.HTTPStatus, "reason", result.Reason)
		f.Method = next
	}
	if errors.Is(err, errNoArticleContent) {
		f.Stopped = true
		f.StopReason = "no_article_content"
		f.FailureNoticePending = false
		f.Failures = 0
		f.LastFailure = ""
		f.NextAttempt = time.Time{}
		result.Reason = "no_article_content"
		markStopped(id, now)
	} else if err != nil {
		f.Failures++
		if f.Failures >= maxFailures && !f.Stopped {
			f.Stopped = true
			f.StopReason = "fetch_failure_limit"
			f.NextAttempt = time.Time{}
			markStopped(id, now)
		}
	} else {
		f.Failures = 0
	}
	if err == nil {
		outcome = "success"
	} else if f.StopReason == "no_article_content" {
		outcome = "skipped"
	} else if f.Stopped {
		outcome = "stopped"
	} else {
		outcome = "retry"
	}
	if err != nil && f.Stopped && f.StopReason != "no_article_content" {
		f.FailureNoticePending = !optional
		if f.StopReason == "" {
			f.StopReason = f.LastFailure
		}
	}
	r.state.Fetch[id] = f
	// API・Slackの処理より前に取得状態をローカル保存する。シェルが従来どおりpushする。
	if saveErr := r.save(); saveErr != nil {
		return Article{}, fmt.Errorf("%w: %v", errDelivery, saveErr)
	}
	if noticeErr := notify(u); noticeErr != nil {
		return Article{}, noticeErr
	}
	if f.StopReason == "no_article_content" {
		return Article{}, errFetchDeferred
	}
	return a, err
}

func (r *runner) saveFetch(id string, f FetchState) error {
	r.state.Fetch[id] = f
	if err := r.save(); err != nil {
		return fmt.Errorf("%w: %v", errDelivery, err)
	}
	return nil
}

func (r *runner) crawlbaseUnavailable(now time.Time) string {
	if r.crawlbase == nil || r.crawlbase.token == "" {
		return "crawlbase_unconfigured"
	}
	if r.crawlbase.disabled {
		return "crawlbase_disabled"
	}
	if now.Before(r.state.CrawlbaseRetryAfter) {
		return "crawlbase_cooldown"
	}
	if r.crawlbase.used >= r.crawlbase.limit {
		return "crawlbase_limit"
	}
	return ""
}
