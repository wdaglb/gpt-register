package main

import (
	"strings"
	"time"

	"go-register/utils"
)

const (
	chromeMacUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	chromeWinUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	safariMacUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15"
	chromeSecCHUA      = `"Google Chrome";v="135", "Chromium";v="135", "Not.A/Brand";v="24"`
	chromeSecCHUAFull  = `"Google Chrome";v="135.0.0.0", "Chromium";v="135.0.0.0", "Not.A/Brand";v="24.0.0.0"`
)

// browserPersona 固定单个账号流程里的浏览器画像。
// Why: 注册、Sentinel、Turnstile 和 OAuth 都要复用同一套画像，避免同一账号流程里前后出现平台/浏览器漂移。
type browserPersona struct {
	Name                string
	UserAgent           string
	AcceptLanguage      string
	Language            string
	LanguagesJoin       string
	PlatformJS          string
	PlatformHeader      string
	PlatformVersion     string
	Architecture        string
	Bitness             string
	Vendor              string
	SendClientHints     bool
	SecCHUA             string
	SecCHUAFullVersion  string
	ScreenWidth         int
	ScreenHeight        int
	HeapLimit           int64
	HardwareConcurrency int
	TimezoneOffsetMin   int
	SessionID           string
	TimeOrigin          float64
	WindowFlags         [7]int
	NavigatorProbe      string
	DocumentProbe       string
	WindowProbe         string
	EntropyA            float64
	EntropyB            float64
	PerformanceNow      float64
	RequirementsElapsed float64
}

func newBrowserPersona() browserPersona {
	sessionID := strings.ToLower(randomURLToken(16))
	if len(sessionID) > 11 {
		sessionID = sessionID[:11]
	}

	personas := []browserPersona{
		{
			Name:                "mac-chrome-en",
			UserAgent:           chromeMacUserAgent,
			AcceptLanguage:      "en-US,en;q=0.9",
			Language:            "en-US",
			LanguagesJoin:       "en-US,en",
			PlatformJS:          "MacIntel",
			PlatformHeader:      "macOS",
			PlatformVersion:     "15.3.1",
			Architecture:        "x86",
			Bitness:             "64",
			Vendor:              "Google Inc.",
			SendClientHints:     true,
			SecCHUA:             chromeSecCHUA,
			SecCHUAFullVersion:  chromeSecCHUAFull,
			ScreenWidth:         1512,
			ScreenHeight:        982,
			HeapLimit:           4294705152,
			HardwareConcurrency: 8,
			TimezoneOffsetMin:   420,
			NavigatorProbe:      "hardwareConcurrency−8",
			DocumentProbe:       "__reactContainer$" + sessionID,
			WindowProbe:         "__oai_cached_session",
		},
		{
			Name:                "windows-chrome-en",
			UserAgent:           chromeWinUserAgent,
			AcceptLanguage:      "en-US,en;q=0.9",
			Language:            "en-US",
			LanguagesJoin:       "en-US,en",
			PlatformJS:          "Win32",
			PlatformHeader:      "Windows",
			PlatformVersion:     "10.0.0",
			Architecture:        "x86",
			Bitness:             "64",
			Vendor:              "Google Inc.",
			SendClientHints:     true,
			SecCHUA:             chromeSecCHUA,
			SecCHUAFullVersion:  chromeSecCHUAFull,
			ScreenWidth:         1920,
			ScreenHeight:        1080,
			HeapLimit:           4294705152,
			HardwareConcurrency: 8,
			TimezoneOffsetMin:   420,
			NavigatorProbe:      "platform−Win32",
			DocumentProbe:       "__reactContainer$" + sessionID,
			WindowProbe:         "__oai_so_bm",
		},
		{
			Name:                "mac-safari-en",
			UserAgent:           safariMacUserAgent,
			AcceptLanguage:      "en-US,en;q=0.9",
			Language:            "en-US",
			LanguagesJoin:       "en-US,en",
			PlatformJS:          "MacIntel",
			PlatformHeader:      "macOS",
			PlatformVersion:     "15.3.1",
			Architecture:        "x86",
			Bitness:             "64",
			Vendor:              "Apple Computer, Inc.",
			SendClientHints:     false,
			ScreenWidth:         1512,
			ScreenHeight:        982,
			HeapLimit:           4294705152,
			HardwareConcurrency: 8,
			TimezoneOffsetMin:   420,
			NavigatorProbe:      "language−en-US",
			DocumentProbe:       "location",
			WindowProbe:         "onpagehide",
		},
	}

	selected := personas[utils.RandomInt(0, len(personas)-1)]
	selected.SessionID = randomURLToken(16)
	selected.TimeOrigin = float64(time.Now().Add(-time.Duration(utils.RandomInt(5000, 120000)) * time.Millisecond).UnixMilli())
	selected.EntropyA = float64(utils.RandomInt(10000, 99999)) / 100000
	selected.EntropyB = float64(utils.RandomInt(10000, 99999)) / 100000
	selected.PerformanceNow = float64(utils.RandomInt(7000, 16000))
	selected.RequirementsElapsed = float64(utils.RandomInt(150, 900))
	selected.WindowFlags = [][7]int{
		{0, 0, 0, 0, 0, 0, 0},
		{1, 0, 0, 0, 0, 0, 0},
		{0, 1, 0, 0, 0, 0, 0},
	}[utils.RandomInt(0, 2)]
	return selected
}
