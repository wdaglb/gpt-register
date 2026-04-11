package main

import "testing"

func TestParseAccountLineNewFormat(t *testing.T) {
	t.Parallel()

	record, ok := parseAccountLine("demo@example.com----Passw0rd!----ok----2026-04-11 22:00:00----oauth=ok----2026-04-11 22:05:00----auth/codex-demo.json")
	if !ok {
		t.Fatalf("parseAccountLine() ok = false, want true")
	}
	if record.Email != "demo@example.com" {
		t.Fatalf("Email = %q, want %q", record.Email, "demo@example.com")
	}
	if record.RegisterStatus != "ok" {
		t.Fatalf("RegisterStatus = %q, want %q", record.RegisterStatus, "ok")
	}
	if record.OAuthStatus != "oauth=ok" {
		t.Fatalf("OAuthStatus = %q, want %q", record.OAuthStatus, "oauth=ok")
	}
	if record.AuthFilePath != "auth/codex-demo.json" {
		t.Fatalf("AuthFilePath = %q, want %q", record.AuthFilePath, "auth/codex-demo.json")
	}
}

func TestParseAccountLineLegacyOAuthFail(t *testing.T) {
	t.Parallel()

	record, ok := parseAccountLine("legacy@example.com----Passw0rd!----fail----add_phone")
	if !ok {
		t.Fatalf("parseAccountLine() ok = false, want true")
	}
	if record.OAuthStatus != "oauth=fail:add_phone" {
		t.Fatalf("OAuthStatus = %q, want %q", record.OAuthStatus, "oauth=fail:add_phone")
	}
}

func TestSerializeAccountRecord(t *testing.T) {
	t.Parallel()

	line := serializeAccountRecord(accountRecord{
		Email:          "demo@example.com",
		Password:       "Passw0rd!",
		RegisterStatus: "ok",
		RegisterTime:   "2026-04-11 22:00:00",
		OAuthStatus:    "oauth=pending",
	})

	want := "demo@example.com----Passw0rd!----ok----2026-04-11 22:00:00----oauth=pending--------"
	if line != want {
		t.Fatalf("serializeAccountRecord() = %q, want %q", line, want)
	}
}
