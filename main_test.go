package main

import "testing"

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
