package main

import (
	"testing"
	"time"
)

// TestBuildBrowserHeadersChromePersona 验证 Chrome 画像会携带 Chromium UA-CH，并保持英文语言头。
func TestBuildBrowserHeadersChromePersona(t *testing.T) {
	t.Parallel()

	persona := newBrowserPersona()
	persona.UserAgent = chromeMacUserAgent
	persona.AcceptLanguage = "en-US,en;q=0.9"
	persona.SendClientHints = true
	persona.SecCHUA = chromeSecCHUA
	persona.SecCHUAFullVersion = chromeSecCHUAFull
	persona.PlatformHeader = "macOS"
	persona.PlatformVersion = "15.3.1"
	persona.Architecture = "x86"
	persona.Bitness = "64"

	headers := buildBrowserHeaders(requestOptions{
		Method:      "GET",
		URL:         "https://auth.openai.com/log-in",
		Accept:      auth0AcceptHeader,
		Profile:     profileNavigate,
		Referer:     "https://auth.openai.com/",
		Origin:      "https://auth.openai.com",
		ContentType: "application/json",
	}, persona)

	if got := headers["accept-language"][0]; got != "en-US,en;q=0.9" {
		t.Fatalf("accept-language = %q, want %q", got, "en-US,en;q=0.9")
	}
	if headers["sec-ch-ua"][0] == "" {
		t.Fatal("expected sec-ch-ua to be present for chrome persona")
	}
}

// TestBuildBrowserHeadersSafariPersona 验证 Safari 画像不会发送 Chromium 专属 UA-CH。
func TestBuildBrowserHeadersSafariPersona(t *testing.T) {
	t.Parallel()

	persona := newBrowserPersona()
	persona.UserAgent = safariMacUserAgent
	persona.AcceptLanguage = "en-US,en;q=0.9"
	persona.SendClientHints = false
	persona.PlatformHeader = "macOS"

	headers := buildBrowserHeaders(requestOptions{
		Method:  "GET",
		URL:     "https://auth.openai.com/log-in",
		Accept:  auth0AcceptHeader,
		Profile: profileNavigate,
	}, persona)

	if len(headers["sec-ch-ua"]) > 0 {
		t.Fatalf("expected safari persona to omit sec-ch-ua, got %q", headers["sec-ch-ua"][0])
	}
	if len(headers["sec-ch-ua-platform"]) > 0 {
		t.Fatalf("expected safari persona to omit sec-ch-ua-platform, got %q", headers["sec-ch-ua-platform"][0])
	}
}

// TestNewSentinelSessionUsesProtocolPersona 验证 Sentinel 会话会复用 protocolClient 已固定的画像和设备标识。
func TestNewSentinelSessionUsesProtocolPersona(t *testing.T) {
	t.Parallel()

	persona := newBrowserPersona()
	persona.UserAgent = chromeWinUserAgent
	persona.Language = "en-US"
	persona.LanguagesJoin = "en-US,en"
	persona.PlatformJS = "Win32"
	persona.Vendor = "Google Inc."
	persona.ScreenWidth = 1920
	persona.ScreenHeight = 1080
	persona.SessionID = "session-fixed"
	persona.TimeOrigin = 123456789

	client := &protocolClient{
		persona:  persona,
		deviceID: "client-device-id",
	}
	bootstrap := &bootstrapPage{DeviceID: "bootstrap-device-id"}

	session, err := newSentinelSession(config{requestTimeout: time.Second}, client, bootstrap)
	if err != nil {
		t.Fatalf("newSentinelSession() error = %v", err)
	}
	if session.DeviceID != "bootstrap-device-id" {
		t.Fatalf("DeviceID = %q, want %q", session.DeviceID, "bootstrap-device-id")
	}
	if session.UserAgent != chromeWinUserAgent {
		t.Fatalf("UserAgent = %q, want %q", session.UserAgent, chromeWinUserAgent)
	}
	if session.Persona.Platform != "Win32" {
		t.Fatalf("Platform = %q, want %q", session.Persona.Platform, "Win32")
	}
	if session.Persona.SessionID != "session-fixed" {
		t.Fatalf("SessionID = %q, want %q", session.Persona.SessionID, "session-fixed")
	}
}
