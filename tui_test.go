package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFormatAverageRegisterSpeedWithoutCompletedTask(t *testing.T) {
	if got := formatAverageRegisterSpeed(time.Time{}, time.Time{}, 0); got != "--" {
		t.Fatalf("expected no data marker, got %q", got)
	}
}

func TestFormatAverageRegisterSpeedUsesCompletedWindow(t *testing.T) {
	startedAt := time.Date(2026, 4, 11, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(9 * time.Second)

	if got := formatAverageRegisterSpeed(startedAt, finishedAt, 3); got != "3.0秒/个" {
		t.Fatalf("expected 3.0秒/个, got %q", got)
	}
}

func TestBuildTUIRunConfig(t *testing.T) {
	base := config{
		mode:             modeRegister,
		webMailURL:       "http://127.0.0.1:8030",
		authDir:          "auth",
		accountsFile:     "accounts.txt",
		proxy:            "http://127.0.0.1:7890",
		mailbox:          "Junk",
		count:            1,
		workers:          1,
		authorizeWorkers: 1,
		overallTimeout:   4 * time.Minute,
		otpTimeout:       90 * time.Second,
		pollInterval:     3 * time.Second,
		requestTimeout:   20 * time.Second,
	}

	cfg, err := buildTUIRunConfig(
		base,
		modePipeline,
		"http://127.0.0.1:8030",
		"demo@example.com",
		"password",
		"user.txt",
		"auth",
		"accounts.txt",
		"http://127.0.0.1:7890",
		"Junk",
		"20",
		"3",
		"5",
		"5m",
		"2m",
		"5s",
		"30s",
	)
	if err != nil {
		t.Fatalf("expected config build success, got error: %v", err)
	}

	if cfg.mode != modePipeline || cfg.count != 20 || cfg.workers != 3 || cfg.authorizeWorkers != 5 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.webMailURL != "http://127.0.0.1:8030" || cfg.email != "demo@example.com" || cfg.password != "password" || cfg.userFile != "user.txt" {
		t.Fatalf("unexpected config strings: %+v", cfg)
	}
	if cfg.overallTimeout != 5*time.Minute || cfg.otpTimeout != 2*time.Minute || cfg.pollInterval != 5*time.Second || cfg.requestTimeout != 30*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestBuildTUIRunConfigRejectsInvalidValue(t *testing.T) {
	base := config{
		webMailURL:     "http://127.0.0.1:8030",
		authDir:        "auth",
		accountsFile:   "accounts.txt",
		mailbox:        "Junk",
		overallTimeout: 4 * time.Minute,
		otpTimeout:     90 * time.Second,
		pollInterval:   3 * time.Second,
		requestTimeout: 20 * time.Second,
	}
	_, err := buildTUIRunConfig(base, modeRegister, base.webMailURL, "", "", "", base.authDir, base.accountsFile, "", base.mailbox, "0", "1", "1", "4m", "90s", "3s", "20s")
	if err == nil {
		t.Fatal("expected config build error")
	}
}

func TestPersistedTUIConfigRoundTrip(t *testing.T) {
	base := config{
		mode:             modeRegister,
		webMailURL:       "http://127.0.0.1:8030",
		email:            "base@example.com",
		password:         "base",
		userFile:         "user.txt",
		authDir:          "auth",
		accountsFile:     "accounts.txt",
		proxy:            "http://127.0.0.1:7890",
		mailbox:          "Junk",
		count:            1,
		workers:          1,
		authorizeWorkers: 1,
		overallTimeout:   4 * time.Minute,
		otpTimeout:       90 * time.Second,
		pollInterval:     3 * time.Second,
		requestTimeout:   20 * time.Second,
	}
	want := config{
		mode:             modePipeline,
		webMailURL:       "http://127.0.0.1:8040",
		email:            "demo@example.com",
		password:         "secret",
		userFile:         "demo.txt",
		authDir:          "custom-auth",
		accountsFile:     "custom-accounts.txt",
		proxy:            "http://127.0.0.1:8899",
		mailbox:          "INBOX",
		count:            12,
		workers:          3,
		authorizeWorkers: 4,
		overallTimeout:   6 * time.Minute,
		otpTimeout:       2 * time.Minute,
		pollInterval:     6 * time.Second,
		requestTimeout:   45 * time.Second,
	}

	path := filepath.Join(t.TempDir(), ".config.json")
	if err := savePersistedTUIConfigToPath(path, want); err != nil {
		t.Fatalf("expected save success, got error: %v", err)
	}

	got, err := loadPersistedTUIConfigFromPath(path, base)
	if err != nil {
		t.Fatalf("expected load success, got error: %v", err)
	}

	if got.mode != want.mode || got.count != want.count || got.workers != want.workers || got.authorizeWorkers != want.authorizeWorkers {
		t.Fatalf("unexpected persisted config: %+v", got)
	}
	if got.webMailURL != want.webMailURL || got.email != want.email || got.password != want.password || got.userFile != want.userFile {
		t.Fatalf("unexpected persisted strings: %+v", got)
	}
	if got.authDir != want.authDir || got.accountsFile != want.accountsFile || got.proxy != want.proxy || got.mailbox != want.mailbox {
		t.Fatalf("unexpected persisted routing config: %+v", got)
	}
	if got.overallTimeout != want.overallTimeout || got.otpTimeout != want.otpTimeout || got.pollInterval != want.pollInterval || got.requestTimeout != want.requestTimeout {
		t.Fatalf("unexpected persisted config: %+v", got)
	}
}

func TestLoadPersistedTUIConfigRejectsInvalidMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".config.json")
	content := []byte("{\"mode\":\"bad-mode\",\"count\":2,\"workers\":2,\"authorize-workers\":2,\"timeout\":\"4m\",\"otp-timeout\":\"90s\",\"poll-interval\":\"3s\",\"request-timeout\":\"20s\"}\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("expected fixture write success, got error: %v", err)
	}

	_, err := loadPersistedTUIConfigFromPath(path, config{
		webMailURL:     "http://127.0.0.1:8030",
		authDir:        "auth",
		accountsFile:   "accounts.txt",
		mailbox:        "Junk",
		overallTimeout: 4 * time.Minute,
		otpTimeout:     90 * time.Second,
		pollInterval:   3 * time.Second,
		requestTimeout: 20 * time.Second,
	})
	if err == nil {
		t.Fatal("expected load error")
	}
}

func TestDisplayRunMode(t *testing.T) {
	cases := map[runMode]string{
		modeRegister:  "仅注册",
		modeAuthorize: "仅授权",
		modePipeline:  "注册+授权",
		modeLogin:     "登录调试",
	}

	for mode, want := range cases {
		if got := displayRunMode(mode); got != want {
			t.Fatalf("mode %s expected %q, got %q", mode, want, got)
		}
	}
}

func TestDisplayModeConfigHint(t *testing.T) {
	cases := map[runMode]string{
		modeRegister:  "当前模式从邮箱池租用账号注册；“登录邮箱/登录密码/账号文件”不会参与注册流程。",
		modeAuthorize: "当前模式从 accounts.txt 读取待授权账号；“登录邮箱/登录密码/账号文件”不会参与授权流程。",
		modePipeline:  "当前模式会在同一 worker 内串行执行注册+授权；“授权并发”仅为兼容旧配置保留，不再拆分账号内授权线程。",
		modeLogin:     "当前模式只调试单账号登录；优先使用“登录邮箱/登录密码”，未填写时才回退到“账号文件”。",
	}

	for mode, want := range cases {
		if got := displayModeConfigHint(mode); got != want {
			t.Fatalf("mode %s expected %q, got %q", mode, want, got)
		}
	}
}

func TestLoginCredentialFieldHint(t *testing.T) {
	if got := loginCredentialFieldHint(modePipeline, "email"); got != "当前模式忽略" {
		t.Fatalf("expected ignored hint, got %q", got)
	}
	if got := loginCredentialFieldHint(modeLogin, "user-file"); got != "仅登录调试使用；未填写邮箱/密码时回退读取" {
		t.Fatalf("unexpected login mode hint: %q", got)
	}
}

func TestWorkerCardLayoutForMode(t *testing.T) {
	cases := []struct {
		name            string
		mode            runMode
		workers         int
		authorizeWorker int
		wantRegister    int
		wantAuthorize   int
	}{
		{name: "register", mode: modeRegister, workers: 3, authorizeWorker: 2, wantRegister: 3, wantAuthorize: 0},
		{name: "authorize", mode: modeAuthorize, workers: 4, authorizeWorker: 2, wantRegister: 0, wantAuthorize: 4},
		{name: "pipeline", mode: modePipeline, workers: 2, authorizeWorker: 5, wantRegister: 2, wantAuthorize: 0},
		{name: "login", mode: modeLogin, workers: 2, authorizeWorker: 5, wantRegister: 0, wantAuthorize: 0},
	}

	for _, testCase := range cases {
		gotRegister, gotAuthorize := workerCardLayoutForMode(testCase.mode, testCase.workers, testCase.authorizeWorker)
		if gotRegister != testCase.wantRegister || gotAuthorize != testCase.wantAuthorize {
			t.Fatalf("%s expected (%d,%d), got (%d,%d)", testCase.name, testCase.wantRegister, testCase.wantAuthorize, gotRegister, gotAuthorize)
		}
	}
}

func TestNewRunTUIModelStartsAtHomeWithPreviewCards(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          2,
		authorizeWorkers: 3,
	}, make(chan struct{}), make(chan config), nil)

	if model.phase != tuiPhaseHome {
		t.Fatalf("expected home phase, got %q", model.phase)
	}
	if len(model.cardOrder) != 1 {
		t.Fatalf("expected only system log card, got %d cards", len(model.cardOrder))
	}
	if _, ok := model.logCards[tuiSystemCardID]; !ok {
		t.Fatal("expected system log card")
	}
}

func TestSwitchToHomePageRebuildsPreviewCardsFromForm(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.modeIndex = tuiModeIndex(modeAuthorize)
	model.workersInput.SetValue("4")

	model.switchToHomePage()

	if model.phase != tuiPhaseHome {
		t.Fatalf("expected home phase, got %q", model.phase)
	}
	if len(model.cardOrder) != 1 {
		t.Fatalf("expected home to keep only system card, got %d cards", len(model.cardOrder))
	}
}

func TestTUIFinishedMsgDoesNotForceQuit(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.started = true
	model.running = true

	updated, cmd := model.Update(tuiFinishedMsg{})
	finalModel := updated.(*runTUIModel)

	if finalModel == nil {
		t.Fatal("expected updated model")
	}
	if !finalModel.finished {
		t.Fatal("expected model marked as finished")
	}
	if finalModel.running {
		t.Fatal("expected model no longer running after finished message")
	}
	if cmd != nil {
		t.Fatal("expected no quit command after finished message")
	}
}

func TestFooterHintAfterTaskFinished(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.started = true
	model.finished = true

	if got := model.footerHint(); got != "任务已结束：Tab/左右切换按钮，Enter 执行，上下滚日志。" {
		t.Fatalf("unexpected footer hint: %q", got)
	}
}

func TestWorkerSummaryText(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          2,
		authorizeWorkers: 3,
	}, make(chan struct{}), make(chan config), nil)

	if got := model.workerSummaryText(); got != "注册线程=2，授权线程=0" {
		t.Fatalf("unexpected worker summary: %q", got)
	}
}

func TestFooterViewContainsWorkerSummary(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          2,
		authorizeWorkers: 3,
	}, make(chan struct{}), make(chan config), nil)

	rendered := model.footerView()
	if !strings.Contains(rendered, "注册线程=2，授权线程=0") {
		t.Fatalf("expected footer to contain worker summary, got %q", rendered)
	}
}

func TestFooterViewContainsEmailPoolSummary(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          2,
		authorizeWorkers: 3,
	}, make(chan struct{}), make(chan config), nil)
	model.emailPoolStatsLoaded = true
	model.emailPoolStats = emailPoolStats{
		Total:     10,
		Available: 4,
		Leased:    2,
		Used:      3,
	}

	rendered := model.footerView()
	if !strings.Contains(rendered, "可用=4，已使用=3，未使用=7，租用中=2") {
		t.Fatalf("expected footer to contain email pool summary, got %q", rendered)
	}
}

func TestFooterViewContainsVersion(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          2,
		authorizeWorkers: 3,
	}, make(chan struct{}), make(chan config), nil)
	model.width = 120

	rendered := model.footerView()
	if !strings.Contains(rendered, "版本 "+appVersion) {
		t.Fatalf("expected footer to contain version, got %q", rendered)
	}
}

func TestUpdateHomePhaseAllowsConfigAfterTaskFinished(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.finished = true
	model.homeActionIndex = tuiHomeActionConfig

	_ = model.updateHomePhase(tea.KeyMsg{Type: tea.KeyEnter})

	if model.phase != tuiPhaseConfig {
		t.Fatalf("expected config phase, got %q", model.phase)
	}
}

func TestUpdateHomePhaseTabCyclesHomeAction(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modePipeline,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	initial := model.homeActionIndex
	_ = model.updateHomePhase(tea.KeyMsg{Type: tea.KeyTab})
	if model.homeActionIndex == initial {
		t.Fatalf("expected home action to change, still %d", model.homeActionIndex)
	}
}

func TestFormatWorkerCardLogRowsKeepsFixedWindow(t *testing.T) {
	rows := formatWorkerCardLogRows([]string{
		"第一条日志",
		"第二条日志",
		"第三条日志",
		"第四条日志",
		"第五条日志",
	}, 4, 40, 0)

	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	if rows[0] != "↑ 第二条日志" {
		t.Fatalf("unexpected first row: %q", rows[0])
	}
	if rows[3] != "• 第五条日志" {
		t.Fatalf("unexpected last row: %q", rows[3])
	}
}

func TestFormatWorkerCardLogRowsSupportsOffset(t *testing.T) {
	rows := formatWorkerCardLogRows([]string{
		"第一条日志",
		"第二条日志",
		"第三条日志",
		"第四条日志",
		"第五条日志",
	}, 3, 40, 1)

	if rows[0] != "↑ 第二条日志" {
		t.Fatalf("unexpected first row: %q", rows[0])
	}
	if rows[2] != "↓ 第四条日志" {
		t.Fatalf("unexpected last row: %q", rows[2])
	}
}

func TestClipCardLogLineAvoidsWrapping(t *testing.T) {
	got := clipCardLogLine("• ", "这是一条很长很长的日志内容，需要被截断以避免卡片换行", 14)
	if got != "• 这是一条很长很长的日志…" {
		t.Fatalf("unexpected clipped line: %q", got)
	}
}

func TestScrollFocusedCardClampsOffset(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.height = 32

	card := model.ensureLogCard("worker-1", "注册 Worker 1", "")
	card.Logs = []string{"1", "2", "3", "4", "5", "6", "7"}
	model.focusedCardID = "worker-1"

	model.scrollFocusedCard(99)
	if card.LogOffset != 3 {
		t.Fatalf("expected clamped offset 3, got %d", card.LogOffset)
	}

	model.scrollFocusedCard(-99)
	if card.LogOffset != 0 {
		t.Fatalf("expected clamped offset 0, got %d", card.LogOffset)
	}
}

func TestScrollFocusedCardSupportsSystemCard(t *testing.T) {
	model := newRunTUIModel(config{
		mode:             modeRegister,
		workers:          1,
		authorizeWorkers: 1,
	}, make(chan struct{}), make(chan config), nil)
	model.height = 32

	card := model.ensureLogCard(tuiSystemCardID, "系统日志", "")
	card.Logs = []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	model.focusedCardID = tuiSystemCardID

	model.scrollFocusedCard(99)
	if card.LogOffset != 4 {
		t.Fatalf("expected system card clamped offset 4, got %d", card.LogOffset)
	}
}

func TestClassifyTUILogLineForRegisterWorker(t *testing.T) {
	cardID, title, subtitle, displayLine := classifyTUILogLine("[go-register] 2026/04/12 10:00:00 [worker-2][demo@example.com] 注册成功")

	if cardID != tuiSystemCardID {
		t.Fatalf("expected system card, got %q", cardID)
	}
	if title != "系统日志" {
		t.Fatalf("expected 系统日志 title, got %q", title)
	}
	if subtitle != "汇总所有运行日志" {
		t.Fatalf("expected system subtitle, got %q", subtitle)
	}
	if displayLine != "2026/04/12 10:00:00 [worker-2][demo@example.com] 注册成功" {
		t.Fatalf("unexpected display line: %q", displayLine)
	}
}

func TestClassifyTUILogLineForSystemCard(t *testing.T) {
	cardID, title, subtitle, displayLine := classifyTUILogLine("[go-register] 2026/04/12 10:00:00 执行完成")

	if cardID != tuiSystemCardID {
		t.Fatalf("expected system card, got %q", cardID)
	}
	if title != "系统日志" {
		t.Fatalf("expected 系统日志 title, got %q", title)
	}
	if subtitle != "汇总所有运行日志" {
		t.Fatalf("unexpected subtitle: %q", subtitle)
	}
	if displayLine != "2026/04/12 10:00:00 执行完成" {
		t.Fatalf("unexpected display line: %q", displayLine)
	}
}

func TestParseLogCardIdentity(t *testing.T) {
	kind, workerIndex := parseLogCardIdentity("auth-3")
	if kind != "auth" || workerIndex != "3" {
		t.Fatalf("unexpected identity: kind=%q workerIndex=%q", kind, workerIndex)
	}
}

func TestInferWorkerStatus(t *testing.T) {
	if got := inferWorkerStatus("2026/04/12 10:00:00 注册成功", ""); got != "注册成功" {
		t.Fatalf("expected 注册成功, got %q", got)
	}
	if got := inferWorkerStatus("2026/04/12 10:00:01 未命中规则", "进行中"); got != "进行中" {
		t.Fatalf("expected fallback status, got %q", got)
	}
}
