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

func TestExtractFirstIPFromAPIIPCCJSON(t *testing.T) {
	t.Parallel()

	body := `{"ip":"203.27.106.146","country_code":"SG","city":"","country":"Singapore","province":"","zip_code":"","timezone":"Asia/Singapore","latitude":1.35208,"longitude":103.82,"asn":"AS137409","asn_name":"GSL Networks Pty LTD","asn_type":"hosting"}`
	got := extractFirstIP(body)
	if got != "203.27.106.146" {
		t.Fatalf("extractFirstIP() = %q, want %q", got, "203.27.106.146")
	}
}

func TestExtractFirstIPFromPlainText(t *testing.T) {
	t.Parallel()

	got := extractFirstIP("120.235.116.234\n")
	if got != "120.235.116.234" {
		t.Fatalf("extractFirstIP() = %q, want %q", got, "120.235.116.234")
	}
}
