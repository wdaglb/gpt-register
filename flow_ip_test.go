package main

import "testing"

func TestBuildFlowLogPrefix(t *testing.T) {
	t.Parallel()

	got := buildFlowLogPrefix("worker-1", "1.2.3.4", "demo@example.com")
	want := "[worker-1][1.2.3.4][demo@example.com]"
	if got != want {
		t.Fatalf("buildFlowLogPrefix() = %q, want %q", got, want)
	}
}

func TestBuildFlowLogPrefixFallsBackToUnknownIP(t *testing.T) {
	t.Parallel()

	got := buildFlowLogPrefix("auth-1", "", "")
	want := "[auth-1][" + flowUnknownIP + "]"
	if got != want {
		t.Fatalf("buildFlowLogPrefix() = %q, want %q", got, want)
	}
}
