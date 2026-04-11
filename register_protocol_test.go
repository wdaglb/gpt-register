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
