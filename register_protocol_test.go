package main

import (
	"errors"
	"testing"
)

func TestShouldKeepRegisterLeaseOnInvalidAuthStep(t *testing.T) {
	err := newFlowResultError("user_register", errors.New(`设置注册密码返回异常状态: 400 body={"error":{"code":"invalid_auth_step"}}`))

	if !shouldKeepRegisterLeaseOnInvalidAuthStep(err) {
		t.Fatal("expected invalid_auth_step during user_register to keep lease out of pool")
	}
}

func TestShouldKeepRegisterLeaseOnInvalidAuthStepRejectsOtherFailures(t *testing.T) {
	cases := []error{
		newFlowResultError("user_register", errors.New(`设置注册密码返回异常状态: 400 body={"error":{"code":"other_code"}}`)),
		newFlowResultError("send_otp", errors.New(`触发邮箱验证码返回异常状态: 400 body={"error":{"code":"invalid_auth_step"}}`)),
		errors.New("plain error"),
	}

	for _, testErr := range cases {
		if shouldKeepRegisterLeaseOnInvalidAuthStep(testErr) {
			t.Fatalf("did not expect helper to keep lease for error: %v", testErr)
		}
	}
}

func TestIsRegisteredEmailFromIdentifierResultAcceptsPasswordStep(t *testing.T) {
	result := &httpResult{
		Body: `{"page":{"type":"login_password"},"continue_url":"https://auth.openai.com/u/login/password?state=abc"}}`,
	}

	if !isRegisteredEmailFromIdentifierResult(result) {
		t.Fatal("expected login password step to confirm registered email")
	}
}

func TestIsRegisteredEmailFromIdentifierResultAcceptsPasswordURL(t *testing.T) {
	result := &httpResult{
		FinalURL: "https://auth.openai.com/u/login/password?state=abc",
	}

	if !isRegisteredEmailFromIdentifierResult(result) {
		t.Fatal("expected password URL to confirm registered email")
	}
}

func TestIsRegisteredEmailFromIdentifierResultRejectsUnknownStep(t *testing.T) {
	result := &httpResult{
		Body: `{"page":{"type":"unknown"},"continue_url":"https://auth.openai.com/create-account/verify-email"}`,
	}

	if isRegisteredEmailFromIdentifierResult(result) {
		t.Fatal("did not expect unknown step to confirm registered email")
	}
}

func TestShouldDispatchPipelineRegisterJob(t *testing.T) {
	cases := []struct {
		name    string
		target  int
		success int
		inFlight int
		want    bool
	}{
		{name: "needs refill", target: 5, success: 2, inFlight: 1, want: true},
		{name: "already saturated", target: 5, success: 2, inFlight: 3, want: false},
		{name: "already reached target", target: 5, success: 5, inFlight: 0, want: false},
	}

	for _, tc := range cases {
		if got := shouldDispatchPipelineRegisterJob(tc.target, tc.success, tc.inFlight); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsNoAvailableLeaseError(t *testing.T) {
	if !isNoAvailableLeaseError(errors.New("调用 web_mail 失败: 当前没有可用邮箱账号")) {
		t.Fatal("expected no available lease error to stop pipeline register")
	}
	if isNoAvailableLeaseError(errors.New("调用 web_mail 失败: timeout")) {
		t.Fatal("did not expect timeout lease error to stop pipeline register")
	}
}
