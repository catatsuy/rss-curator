package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/auth"
	"github.com/openai/openai-go/v3/option"
)

// GitHubのOIDCトークンだけを取得する。OpenAIへの交換と有効期限管理はSDKが行う。
type githubTokenProvider struct {
	requestURL, requestToken, audience string
	client                             *http.Client
}

func (*githubTokenProvider) TokenType() auth.SubjectTokenType { return auth.SubjectTokenTypeJWT }
func (p *githubTokenProvider) GetToken(ctx context.Context, _ auth.HTTPDoer) (token string, err error) {
	defer func() {
		if err != nil {
			err = &auth.SubjectTokenProviderError{Provider: "GitHub", Cause: err}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u, e := url.Parse(p.requestURL)
	if e != nil {
		return "", fmt.Errorf("GitHub OIDCの取得先URLが不正です")
	}
	q := u.Query()
	q.Set("audience", p.audience)
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return "", fmt.Errorf("GitHub OIDCのリクエストを作成できません")
	}
	req.Header.Set("Authorization", "bearer "+p.requestToken)
	resp, e := p.client.Do(req)
	if e != nil {
		return "", fmt.Errorf("GitHub OIDCトークンを取得できません")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub OIDCのHTTPステータス: %d", resp.StatusCode)
	}
	const limit = 1 << 20
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || len(b) > limit {
		return "", fmt.Errorf("GitHub OIDCの応答を読み込めません")
	}
	var result struct {
		Value string `json:"value"`
	}
	if e = json.Unmarshal(b, &result); e != nil || strings.TrimSpace(result.Value) == "" {
		return "", fmt.Errorf("GitHub OIDCの応答に有効なトークンがありません")
	}
	return result.Value, nil
}
func openAIAuthOption() (option.RequestOption, error) {
	providerID := os.Getenv("OPENAI_IDENTITY_PROVIDER_ID")
	accountID := os.Getenv("OPENAI_SERVICE_ACCOUNT_ID")
	audience := os.Getenv("OPENAI_WIF_AUDIENCE")
	// ActionsやWIF設定がある場合、設定漏れをAPIキーで隠さない。
	if os.Getenv("GITHUB_ACTIONS") == "true" || providerID != "" || accountID != "" || audience != "" {
		for _, key := range []string{"OPENAI_IDENTITY_PROVIDER_ID", "OPENAI_SERVICE_ACCOUNT_ID", "OPENAI_WIF_AUDIENCE", "ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
			if strings.TrimSpace(os.Getenv(key)) == "" {
				return nil, fmt.Errorf("認証設定の%sがありません。Workload Identityを使うActionsではid-token: writeも設定してください", key)
			}
		}
		requestURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
		u, e := url.Parse(requestURL)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("GitHub OIDCの取得先には有効なHTTPS URLが必要です")
		}
		provider := &githubTokenProvider{requestURL: requestURL, requestToken: os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"), audience: audience, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		return option.WithWorkloadIdentity(auth.WorkloadIdentity{IdentityProviderID: providerID, ServiceAccountID: accountID, Provider: provider}), nil
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		return nil, fmt.Errorf("ローカル実行にはOPENAI_API_KEYを設定してください。ActionsではWorkload Identity認証を使用します")
	}
	return option.WithAPIKey(os.Getenv("OPENAI_API_KEY")), nil
}
