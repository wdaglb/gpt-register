package main

import (
	"regexp"
	"strings"
	"testing"

	"go-register/utils"
)

func TestExtractOTP(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"data": map[string]any{
			"subject": "OpenAI verification code: 123456",
			"html":    "<strong>123456</strong>",
		},
	}

	code := extractOTP(payload)
	if code != "123456" {
		t.Fatalf("extractOTP() = %q, want %q", code, "123456")
	}
}

func TestExtractOTPWithoutCode(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"data": map[string]any{
			"subject": "Welcome to OpenAI",
			"text":    "This message has no verification token.",
		},
	}

	code := extractOTP(payload)
	if code != "" {
		t.Fatalf("extractOTP() = %q, want empty string", code)
	}
}

func TestExtractState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "query string",
			input: "https://auth0.openai.com/u/login/password?state=abc123",
			want:  "abc123",
		},
		{
			name:  "html fragment",
			input: `<input type="hidden" name="state" value="state-456">`,
			want:  "state-456",
		},
		{
			name:  "url encoded",
			input: "https://example.com/callback?foo=bar&state=hello%2Bworld",
			want:  "hello+world",
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := extractState(testCase.input)
			if got != testCase.want {
				t.Fatalf("extractState() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestNeedsEmailOTP(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"https://auth.openai.com/mfa-challenge/email-otp",
		"https://auth.openai.com/u/email-verification",
		"https://auth.openai.com/flow?screen=email-otp",
	}
	for _, input := range inputs {
		if !needsEmailOTP(input) {
			t.Fatalf("needsEmailOTP(%q) = false, want true", input)
		}
	}
}

func TestResolveRuntimeProxyConfigReplacesPlaceholder(t *testing.T) {
	cfg := config{
		proxy: "http://user-{}:pass@proxy.example.com:8888",
	}

	resolved := cfg
	resolved.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
	if strings.Contains(resolved.proxy, "{}") {
		t.Fatalf("expected placeholder replaced, got %q", resolved.proxy)
	}
	if !strings.HasPrefix(resolved.proxy, "http://user-") || !strings.Contains(resolved.proxy, ":pass@proxy.example.com:8888") {
		t.Fatalf("unexpected proxy format: %q", resolved.proxy)
	}

	matcher := regexp.MustCompile(`^http://user-([A-Za-z0-9]+):pass@proxy\.example\.com:8888$`)
	matches := matcher.FindStringSubmatch(resolved.proxy)
	if len(matches) != 2 {
		t.Fatalf("expected alphanumeric session key, got %q", resolved.proxy)
	}
	if len(matches[1]) != 12 {
		t.Fatalf("expected session key length 12, got %d", len(matches[1]))
	}
}

func TestResolveRuntimeProxyConfigUsesSingleSessionKeyPerRun(t *testing.T) {
	cfg := config{
		proxy: "http://user-{}:pass-{}@proxy.example.com:8888",
	}

	resolved := cfg
	resolved.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
	matcher := regexp.MustCompile(`^http://user-([A-Za-z0-9]+):pass-([A-Za-z0-9]+)@proxy\.example\.com:8888$`)
	matches := matcher.FindStringSubmatch(resolved.proxy)
	if len(matches) != 3 {
		t.Fatalf("unexpected proxy format: %q", resolved.proxy)
	}
	if matches[1] != matches[2] {
		t.Fatalf("expected same session key reused in one proxy string, got %q and %q", matches[1], matches[2])
	}
}

func TestResolveRuntimeProxyConfigUsesLengthFromPlaceholder(t *testing.T) {
	cfg := config{
		proxy: "http://user-{6}:pass-{8}@proxy.example.com:8888",
	}

	resolved := cfg
	resolved.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
	matcher := regexp.MustCompile(`^http://user-([A-Za-z0-9]+):pass-([A-Za-z0-9]+)@proxy\.example\.com:8888$`)
	matches := matcher.FindStringSubmatch(resolved.proxy)
	if len(matches) != 3 {
		t.Fatalf("unexpected proxy format: %q", resolved.proxy)
	}
	if len(matches[1]) != 6 {
		t.Fatalf("expected first session key length 6, got %d", len(matches[1]))
	}
	if len(matches[2]) != 8 {
		t.Fatalf("expected second session key length 8, got %d", len(matches[2]))
	}
}

func TestResolveRuntimeProxyConfigReusesSessionKeyForSameLength(t *testing.T) {
	cfg := config{
		proxy: "http://user-{6}:pass-{6}@proxy.example.com:8888",
	}

	resolved := cfg
	resolved.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
	matcher := regexp.MustCompile(`^http://user-([A-Za-z0-9]+):pass-([A-Za-z0-9]+)@proxy\.example\.com:8888$`)
	matches := matcher.FindStringSubmatch(resolved.proxy)
	if len(matches) != 3 {
		t.Fatalf("unexpected proxy format: %q", resolved.proxy)
	}
	if matches[1] != matches[2] {
		t.Fatalf("expected same-length placeholders to reuse one key, got %q and %q", matches[1], matches[2])
	}
}
