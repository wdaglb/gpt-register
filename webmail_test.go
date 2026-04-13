package main

import (
	"testing"
	"time"
)

func TestMaybeReportWaitProgressReportsEveryInterval(t *testing.T) {
	var reported []time.Duration
	lastReported := time.Duration(0)
	startedAt := time.Now().Add(-3200 * time.Millisecond)

	maybeReportWaitProgress(startedAt, &lastReported, time.Second, func(elapsed time.Duration) {
		reported = append(reported, elapsed)
	})
	if len(reported) != 1 || reported[0] != 3*time.Second {
		t.Fatalf("expected first report at 3s, got %v", reported)
	}

	maybeReportWaitProgress(startedAt, &lastReported, time.Second, func(elapsed time.Duration) {
		reported = append(reported, elapsed)
	})
	if len(reported) != 1 {
		t.Fatalf("expected duplicate report suppressed, got %v", reported)
	}
}

func TestMaybeReportWaitProgressSkipsBeforeThreshold(t *testing.T) {
	called := false
	lastReported := time.Duration(0)

	maybeReportWaitProgress(time.Now().Add(-500*time.Millisecond), &lastReported, time.Second, func(time.Duration) {
		called = true
	})
	if called {
		t.Fatal("expected no progress callback before threshold")
	}
}
