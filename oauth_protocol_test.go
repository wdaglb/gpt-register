package main

import (
	"encoding/json"
	"testing"
)

// TestExtractOAuthCallbackURL 验证协议层能从最终回调地址中提取 code/state。
func TestExtractOAuthCallbackURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "localhost callback",
			input: "http://localhost:1455/auth/callback?code=code-1&state=state-1",
			want:  "http://localhost:1455/auth/callback?code=code-1&state=state-1",
		},
		{
			name:  "external callback",
			input: "https://chatgpt.com/api/auth/callback/openai?code=code-2&state=state-2",
			want:  "https://chatgpt.com/api/auth/callback/openai?code=code-2&state=state-2",
		},
		{
			name:  "consent page is not callback",
			input: "https://auth.openai.com/sign-in-with-chatgpt/codex/consent",
			want:  "",
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := extractOAuthCallbackURL(testCase.input)
			if got != testCase.want {
				t.Fatalf("extractOAuthCallbackURL() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestExtractCallbackURLFromEnvelope 验证 envelope 中的 payload.url / continue_url 可以直接作为 callback 来源。
func TestExtractCallbackURLFromEnvelope(t *testing.T) {
	t.Parallel()

	want := "https://chatgpt.com/api/auth/callback/openai?code=code-3&state=state-3"
	step := authAPIEnvelope{
		ContinueURL: "https://auth.openai.com/unused",
	}
	step.Page.Type = "external_url"
	step.Page.Payload.URL = want

	got := extractCallbackURLFromEnvelope(step)
	if got != want {
		t.Fatalf("extractCallbackURLFromEnvelope() = %q, want %q", got, want)
	}
}

// TestExtractCallbackURLFromHTTPResult 验证提交 consent 后如果响应体里返回 external_url，也能识别出最终 callback。
func TestExtractCallbackURLFromHTTPResult(t *testing.T) {
	t.Parallel()

	want := "https://chatgpt.com/api/auth/callback/openai?code=code-4&state=state-4"
	bodyPayload := map[string]any{
		"continue_url": "https://auth.openai.com/continue",
		"page": map[string]any{
			"type": "external_url",
			"payload": map[string]any{
				"url": want,
			},
		},
	}
	bodyBytes, err := json.Marshal(bodyPayload)
	if err != nil {
		t.Fatalf("expected payload encode success, got error: %v", err)
	}

	result := &httpResult{
		StatusCode: 200,
		Body:       string(bodyBytes),
	}

	got := extractCallbackURLFromHTTPResult(result)
	if got != want {
		t.Fatalf("extractCallbackURLFromHTTPResult() = %q, want %q", got, want)
	}
}

// TestExtractConsentWorkspaceID 验证协议层可以从 consent 原始 HTML 中提取 workspace_id。
func TestExtractConsentWorkspaceID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "rendered form",
			input: `<form method="post" action="/sign-in-with-chatgpt/codex/consent"><input type="hidden" name="workspace_id" value="962351cc-bf5b-4e28-a422-ce0f333f2ff8"></form>`,
			want:  "962351cc-bf5b-4e28-a422-ce0f333f2ff8",
		},
		{
			name:  "react router stream",
			input: `window.__reactRouterContext.streamController.enqueue("[\"session\",{\"workspaces\",[19],{\"id\",\"f614fdfd-8a25-45d1-9b9a-2ed0a0f1a458\",\"kind\",\"personal\"}}]")`,
			want:  "f614fdfd-8a25-45d1-9b9a-2ed0a0f1a458",
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := extractConsentWorkspaceID(testCase.input)
			if got != testCase.want {
				t.Fatalf("extractConsentWorkspaceID() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestExtractOAuthProgressCandidates 验证协议层能从响应体中提取 OAuth 中间跳转地址。
func TestExtractOAuthProgressCandidates(t *testing.T) {
	t.Parallel()

	body := `{"continue_url":"https://auth.openai.com/api/oauth/oauth2/auth?client_id=demo&login_verifier=abc","page":{"type":"external_url","payload":{"url":"https://auth.openai.com/api/accounts/consent?consent_challenge=xyz"}}}`
	result := &httpResult{
		StatusCode: 200,
		FinalURL:   "https://auth.openai.com/sign-in-with-chatgpt/codex/consent",
		Body:       body,
	}

	got := extractOAuthProgressCandidates(result)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 progress candidates, got %#v", got)
	}
	if got[0] != "https://auth.openai.com/api/oauth/oauth2/auth?client_id=demo&login_verifier=abc" && got[1] != "https://auth.openai.com/api/oauth/oauth2/auth?client_id=demo&login_verifier=abc" {
		t.Fatalf("expected oauth2/auth candidate, got %#v", got)
	}
}
