package main

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
)

func TestCollectAuthorizationResults(t *testing.T) {
	results := make(chan authorizationAttemptResult, 3)
	results <- authorizationAttemptResult{Email: "ok@example.com", OAuthStatus: "oauth=ok"}
	results <- authorizationAttemptResult{Email: "fail1@example.com", OAuthStatus: "oauth=fail:otp_wait", Err: errors.New("otp timeout")}
	results <- authorizationAttemptResult{Email: "fail2@example.com", OAuthStatus: "oauth=fail:add_phone", Err: errors.New("add phone")}
	close(results)

	summary := collectAuthorizationResults(results, log.New(io.Discard, "", 0), "pipeline ")
	if summary.success != 1 {
		t.Fatalf("success = %d, want 1", summary.success)
	}
	if summary.fail != 2 {
		t.Fatalf("fail = %d, want 2", summary.fail)
	}
	if summary.firstErr == nil || summary.firstErr.Error() != "otp timeout" {
		t.Fatalf("firstErr = %v, want otp timeout", summary.firstErr)
	}
}

func TestEnqueueAuthorizeJobStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jobs := make(chan accountRecord)
	ok := enqueueAuthorizeJob(ctx, log.New(io.Discard, "", 0), "[worker-1][demo@example.com]", jobs, accountRecord{Email: "demo@example.com"})
	if ok {
		t.Fatal("expected enqueue to stop on canceled context")
	}
}

func TestEnqueueAuthorizeJobSucceedsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan accountRecord, 1)
	record := accountRecord{Email: "demo@example.com"}
	ok := enqueueAuthorizeJob(ctx, log.New(io.Discard, "", 0), "[worker-1][demo@example.com]", jobs, record)
	if !ok {
		t.Fatal("expected enqueue to succeed")
	}

	got := <-jobs
	if got.Email != record.Email {
		t.Fatalf("got email %q, want %q", got.Email, record.Email)
	}
}
