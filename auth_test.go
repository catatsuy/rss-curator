package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/auth"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GITHUB_ACTIONS", "OPENAI_API_KEY", "OPENAI_IDENTITY_PROVIDER_ID", "OPENAI_SERVICE_ACCOUNT_ID", "OPENAI_WIF_AUDIENCE", "ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		t.Setenv(key, "")
	}
}

func TestAuthErrorDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"交換", &auth.OAuthError{StatusCode: 401, ErrorCode: "invalid_grant", ErrorDescription: "secret"}, "HTTP 401、OAuth: invalid_grant"},
		{"未知のコード", &auth.OAuthError{StatusCode: 403, ErrorCode: "secret", ErrorDescription: "secret"}, "HTTP 403、OAuth: unknown"},
		{"OIDC", &auth.SubjectTokenProviderError{Provider: "secret", Message: "secret", Cause: fmt.Errorf("secret")}, "GitHub OIDCトークン取得"},
		{"タイムアウト", context.DeadlineExceeded, "タイムアウト"},
		{"キャンセル", context.Canceled, "キャンセル"},
		{"不明", fmt.Errorf("secret"), "通信に失敗"},
		{"交換HTTP", fmt.Errorf("token exchange failed with status 502"), "トークン交換に失敗しました（HTTP 502）"},
		{"交換JSON", fmt.Errorf("failed to decode token exchange response"), "トークン交換に失敗"},
		{"交換トークン欠落", fmt.Errorf("token exchange response missing 'access_token' field"), "access_token"},
		{"交換HTTPの機密情報", fmt.Errorf("token exchange failed with status 401 secret"), "エラー型:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := apiError("Decisions API", fmt.Errorf("wrapped: %w", tc.err)).Error()
			if !strings.Contains(got, tc.want) || strings.Contains(got, "secret") {
				t.Fatalf("安全な診断情報がありません: %s", got)
			}
		})
	}
}
func TestAuthConfiguration(t *testing.T) {
	clearAuthEnv(t)
	if _, e := openAIAuthOption(); e == nil {
		t.Fatal("認証設定なしで実行できてしまいます")
	}
	t.Setenv("OPENAI_API_KEY", "local-key")
	if _, e := openAIAuthOption(); e != nil {
		t.Fatal(e)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	if _, e := openAIAuthOption(); e == nil {
		t.Fatal("ActionsでWorkload Identity設定なしにAPIキーへフォールバックしました")
	}
	settings := map[string]string{"OPENAI_IDENTITY_PROVIDER_ID": "idp-test", "OPENAI_SERVICE_ACCOUNT_ID": "sa-test", "OPENAI_WIF_AUDIENCE": "https://api.openai.com/v1", "ACTIONS_ID_TOKEN_REQUEST_URL": "https://oidc.example/token?job=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "github-secret"}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if _, e := openAIAuthOption(); e != nil {
		t.Fatal(e)
	}
	t.Setenv("INTEREST_PROMPT", "非公開の判定基準")
	t.Setenv("SUMMARY_PROMPT", "非公開の要約指示")
	if _, e := newAI(); e != nil {
		t.Fatal("APIキーなしでクライアントを初期化できません", e)
	}
	for key, value := range settings {
		t.Setenv(key, "")
		if _, e := openAIAuthOption(); e == nil {
			t.Errorf("設定漏れを検出しません: %s", key)
		}
		t.Setenv(key, value)
	}
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("OPENAI_API_KEY", "local-key")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "http://oidc.example/token")
	if _, e := openAIAuthOption(); e == nil {
		t.Fatal("WIF設定があるのに不正な取得先を受理しました")
	}
}
func TestGitHubTokenProvider(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		fail       bool
	}{{"成功", `{"value":"github-jwt"}`, http.StatusOK, false}, {"空", `{"value":""}`, http.StatusOK, true}, {"不正JSON", `secret-is-not-json`, http.StatusOK, true}, {"権限不足", `github-secret`, http.StatusForbidden, true}, {"大きすぎる応答", strings.Repeat("x", (1<<20)+1), http.StatusOK, true}, {"リダイレクト", "", http.StatusFound, true}} {
		t.Run(tc.name, func(t *testing.T) {
			redirected := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected = true
				}
				if r.Header.Get("Authorization") != "bearer github-secret" {
					t.Error("GitHub用認証ヘッダーがありません")
				}
				if r.URL.Query().Get("audience") != "https://api.openai.com/v1?custom=a&b=c" || r.URL.Query().Get("job") != "1" {
					t.Error("Audienceと既存クエリを保持していません")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			provider := &githubTokenProvider{requestURL: server.URL + "/token?job=1&audience=old", requestToken: "github-secret", audience: "https://api.openai.com/v1?custom=a&b=c", client: client}
			token, e := provider.GetToken(context.Background(), nil)
			if (e != nil) != tc.fail {
				t.Fatalf("トークンの取得結果が不正です: %v", e)
			}
			if !tc.fail && token != "github-jwt" {
				t.Fatal("トークンが一致しません")
			}
			if e != nil && strings.Contains(e.Error(), "github-secret") {
				t.Fatal("エラーに認証情報があります")
			}
			if redirected {
				t.Fatal("リダイレクト先に認証情報を送信しました")
			}
			if provider.TokenType() != auth.SubjectTokenTypeJWT {
				t.Fatal("トークンの型が不正です")
			}
		})
	}
}
func TestWorkloadIdentitySDKExchange(t *testing.T) {
	oidcCalls, exchangeCalls, apiCalls := 0, 0, 0
	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oidcCalls++
		if r.URL.Query().Get("audience") != "https://api.openai.com/v1" {
			t.Error("Audienceが不正です")
		}
		fmt.Fprintf(w, `{"value":"subject-jwt-%d"}`, oidcCalls)
	}))
	defer oidc.Close()
	provider := &githubTokenProvider{requestURL: oidc.URL, requestToken: "github-secret", audience: "https://api.openai.com/v1", client: oidc.Client()}
	client := openai.NewClient(option.WithWorkloadIdentity(auth.WorkloadIdentity{IdentityProviderID: "idp-test", ServiceAccountID: "sa-test", Provider: provider}), option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		if r.URL.String() == auth.TokenExchangeURL {
			exchangeCalls++
			var data map[string]string
			if e := json.NewDecoder(r.Body).Decode(&data); e != nil {
				t.Fatal(e)
			}
			if data["identity_provider_id"] != "idp-test" || data["service_account_id"] != "sa-test" || data["subject_token"] != "subject-jwt-1" || data["subject_token_type"] != auth.JWTTokenType {
				t.Errorf("SDKによるトークン交換の入力が不正です: %v", data)
			}
			body = `{"access_token":"openai-access-token","token_type":"Bearer","expires_in":7200}`
		} else {
			apiCalls++
			if r.Header.Get("Authorization") != "Bearer openai-access-token" {
				t.Error("OpenAI用トークンを使用していません")
			}
			switch r.URL.Path {
			case "/v1/decisions":
				body = `{"answers":[{"type":"score","name":"interest","score":1.5}]}`
			case "/v1/responses":
				body = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"日本語の詳細要約"}]}]}`
			default:
				t.Errorf("想定外のAPI呼び出し: %s", r.URL.Path)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}))
	if _, e := client.Decisions.New(context.Background(), openai.DecisionNewParams{Model: "gpt-6-luna", Input: openai.DecisionNewParamsInputUnion{OfString: openai.String("本文")}, Questions: []openai.DecisionNewParamsQuestionUnion{{OfScore: &openai.DecisionNewParamsQuestionScore{Instructions: "判定", Levels: []openai.DecisionNewParamsQuestionScoreLevel{{Label: "低"}, {Label: "高"}}}}}}); e != nil {
		t.Fatal(e)
	}
	if _, e := client.Responses.New(context.Background(), responses.ResponseNewParams{Model: "gpt-6.1-sol", Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("本文")}}); e != nil {
		t.Fatal(e)
	}
	if oidcCalls != 1 || exchangeCalls != 1 || apiCalls != 2 {
		t.Fatalf("SDKのトークン共有が不正です: OIDC%d 交換%d API%d", oidcCalls, exchangeCalls, apiCalls)
	}
	// 更新が必要になったとき、固定JWTの使い回しではなく新しいJWTを取得できる。
	if token, e := provider.GetToken(context.Background(), nil); e != nil || token != "subject-jwt-2" {
		t.Fatal("OIDCトークンを再取得できません", e)
	}
}
