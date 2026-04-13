package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xterm "github.com/charmbracelet/x/term"
)

const (
	maxTUILogLines                   = 80
	tuiConfigFile                    = ".config.json"
	tuiSystemCardID                  = "system"
	tuiRunningCardGap                = 1
	tuiEmailPoolStatsRefreshInterval = 5 * time.Second

	tuiFocusMode = iota
	tuiFocusWebMailURL
	tuiFocusEmail
	tuiFocusPassword
	tuiFocusUserFile
	tuiFocusAuthDir
	tuiFocusAccountsFile
	tuiFocusCPAURL
	tuiFocusCPAKey
	tuiFocusProxy
	tuiFocusMailbox
	tuiFocusCount
	tuiFocusWorkers
	tuiFocusAuthorizeWorkers
	tuiFocusTimeout
	tuiFocusOTPTimeout
	tuiFocusPollInterval
	tuiFocusRequestTimeout
	tuiFocusSave
	tuiFocusTotal
)

type tuiPhase string

const (
	tuiPhaseHome   tuiPhase = "home"
	tuiPhaseConfig tuiPhase = "config"
)

const (
	tuiHomeActionStart = iota
	tuiHomeActionConfig
	tuiHomeActionTotal
)

var (
	tuiMutedStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	tuiValueStyle          = lipgloss.NewStyle().Bold(true)
	tuiSuccessStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	tuiFailStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Bold(true)
	tuiFooterStyle         = lipgloss.NewStyle().Padding(0, 1)
	tuiTitleStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	tuiCardStyle           = lipgloss.NewStyle().Padding(0, 1)
	tuiFieldLabelStyle     = lipgloss.NewStyle().Width(20).Foreground(lipgloss.Color("248"))
	tuiFocusedLabelStyle   = lipgloss.NewStyle().Width(20).Foreground(lipgloss.Color("86")).Bold(true)
	tuiSelectedModeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1).Bold(true)
	tuiUnselectedModeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	tuiStartButtonStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 2).Bold(true)
	tuiStartIdleStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Padding(0, 1)
	tuiLogCardStyle        = lipgloss.NewStyle().Padding(0, 0)
	tuiFocusedLogCardStyle = tuiLogCardStyle.Copy()
	tuiCardTitleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("117"))
	tuiCardSubtitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

var tuiWorkerLogPattern = regexp.MustCompile(`\[(worker|auth)-(\d+)\](?:\[([^\]]+)\])?`)

var tuiModeOptions = []runMode{
	modeRegister,
	modeAuthorize,
	modePipeline,
	modeLogin,
}

// progressUI 统一抽象日志输出和统计事件上报。
// Why: 普通日志模式与 Bubble Tea TUI 只在“如何显示”上不同，业务层只依赖这一层接口即可复用同一套调度逻辑。
type progressUI interface {
	LogWriter() io.Writer
	RecordRegisterStart()
	RecordRegisterFinish(success bool)
	RecordAuthorizeFinish(success bool)
}

// plainProgressUI 在非交互终端中保留原始日志行为。
// Why: CI、重定向输出或脚本调用时不应强制进入 TUI，避免 ANSI 渲染污染日志文件。
type plainProgressUI struct {
	writer io.Writer
}

func newPlainProgressUI(writer io.Writer) *plainProgressUI {
	return &plainProgressUI{writer: writer}
}

func (ui *plainProgressUI) LogWriter() io.Writer {
	return ui.writer
}

func (ui *plainProgressUI) RecordRegisterStart() {}

func (ui *plainProgressUI) RecordRegisterFinish(success bool) {}

func (ui *plainProgressUI) RecordAuthorizeFinish(success bool) {}

// shouldUseTUI 仅在真实交互终端中启用 Bubble Tea。
// Why: 只有终端具备窗口尺寸与控制能力时，固定底栏和滚动日志区才有意义。
func shouldUseTUI(cfg config) bool {
	if cfg.mode == modeWebMail {
		// Why: webmail 模式本身就是常驻 HTTP 服务，交互式配置页会阻断服务进程启动，因此始终禁用 TUI。
		return false
	}
	if os.Getenv("TERM") == "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return xterm.IsTerminal(os.Stdout.Fd())
}

// runWithTUI 在主 goroutine 中托管 Bubble Tea 事件循环，并在独立配置页提交后启动真实任务。
// Why: 首页现在只负责展示 worker 卡片，真正的参数编辑被收敛到独立配置页，因此执行协程必须等待配置确认后再启动。
func runWithTUI(parent context.Context, cfg config, mailClient *webMailClient, store *accountsStore) error {
	loadedCfg, loadErr := loadPersistedTUIConfig(cfg)

	ready := make(chan struct{})
	startCh := make(chan config, 1)
	model := newRunTUIModel(loadedCfg, ready, startCh, loadErr)
	// Why: 配置页和日志页都属于完整终端界面，占用 alt screen 可以避免和历史终端输出互相穿插。
	program := tea.NewProgram(model, tea.WithAltScreen())
	ui := newBubbleTeaProgressUI(program)
	workerDoneCh := make(chan struct{})
	var lastRunErr error
	var hasRun bool

	go func() {
		defer close(workerDoneCh)
		<-ready

		for runCfg := range startCh {
			logger := log.New(ui.LogWriter(), "[go-register] ", log.LstdFlags|log.Lmsgprefix)
			err := executeMode(parent, runCfg, mailClient, logger, store, ui)
			hasRun = true
			lastRunErr = err

			if err != nil {
				program.Send(tuiLogLineMsg{Line: fmt.Sprintf("执行失败: %v", err)})
			} else {
				program.Send(tuiLogLineMsg{Line: "执行完成"})
			}
			program.Send(tuiFinishedMsg{})
		}
	}()

	returnModel, err := program.Run()
	close(startCh)
	<-workerDoneCh
	if err != nil {
		return err
	}

	finalModel, _ := returnModel.(*runTUIModel)
	if finalModel == nil || !hasRun {
		return nil
	}
	if !finalModel.started {
		return nil
	}
	return lastRunErr
}

// bubbleTeaProgressUI 负责把业务层事件转换成 Bubble Tea 消息。
// Why: logger 与 worker 运行在多个 goroutine 中，统一从这里 Send 消息，可以避免模型层直接接触并发细节。
type bubbleTeaProgressUI struct {
	program *tea.Program
	writer  *bubbleTeaLogWriter
}

func newBubbleTeaProgressUI(program *tea.Program) *bubbleTeaProgressUI {
	return &bubbleTeaProgressUI{
		program: program,
		writer:  &bubbleTeaLogWriter{program: program},
	}
}

func (ui *bubbleTeaProgressUI) LogWriter() io.Writer {
	return ui.writer
}

func (ui *bubbleTeaProgressUI) RecordRegisterStart() {
	ui.program.Send(tuiRegisterStartMsg{StartedAt: time.Now()})
}

func (ui *bubbleTeaProgressUI) RecordRegisterFinish(success bool) {
	ui.program.Send(tuiRegisterFinishMsg{
		Success:    success,
		FinishedAt: time.Now(),
	})
}

func (ui *bubbleTeaProgressUI) RecordAuthorizeFinish(success bool) {
	ui.program.Send(tuiAuthorizeFinishMsg{Success: success})
}

// bubbleTeaLogWriter 把标准 logger 的多行输出拆成单条 TUI 消息。
// Why: log.Logger 只认识 io.Writer，而 TUI 渲染需要拿到已经分割好的日志行，才能稳定维护滚动视图。
type bubbleTeaLogWriter struct {
	program *tea.Program
	mu      sync.Mutex
	pending string
}

func (writer *bubbleTeaLogWriter) Write(payload []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()

	writer.pending += string(payload)
	for {
		index := strings.IndexByte(writer.pending, '\n')
		if index < 0 {
			break
		}

		line := strings.TrimRight(writer.pending[:index], "\r")
		writer.program.Send(tuiLogLineMsg{Line: line})
		writer.pending = writer.pending[index+1:]
	}
	return len(payload), nil
}

type tuiLogLineMsg struct {
	Line string
}

type tuiRegisterStartMsg struct {
	StartedAt time.Time
}

type tuiRegisterFinishMsg struct {
	Success    bool
	FinishedAt time.Time
}

type tuiAuthorizeFinishMsg struct {
	Success bool
}

type tuiEmailPoolStatsMsg struct {
	Stats emailPoolStats
	Err   error
}

type tuiEmailPoolStatsTickMsg struct{}

type tuiFinishedMsg struct{}

// tuiPersistentConfig 只持久化 TUI 页面负责维护的关键运行参数。
// Why: 其它网络和账号字段仍由 CLI/env 控制，单独存这 4 个字段可以避免把用户已有外部配置来源打散。
type tuiPersistentConfig struct {
	Mode             string `json:"mode"`
	WebMailURL       string `json:"web-mail-url"`
	Email            string `json:"email"`
	Password         string `json:"password"`
	UserFile         string `json:"user-file"`
	AuthDir          string `json:"auth-dir"`
	AccountsFile     string `json:"accounts-file"`
	CPAURL           string `json:"cpa-url"`
	CPAKey           string `json:"cpa-key"`
	Proxy            string `json:"proxy"`
	Mailbox          string `json:"mailbox"`
	Count            int    `json:"count"`
	Workers          int    `json:"workers"`
	AuthorizeWorkers int    `json:"authorize-workers"`
	Timeout          string `json:"timeout"`
	OTPTimeout       string `json:"otp-timeout"`
	PollInterval     string `json:"poll-interval"`
	RequestTimeout   string `json:"request-timeout"`
}

// tuiLogCard 表示单个 worker 或系统日志卡片。
// Why: 用户要求把日志按 worker 归档成独立卡片，因此每张卡片都要维护自己的标题、上下文和日志列表。
type tuiLogCard struct {
	ID             string
	Kind           string
	WorkerIndex    string
	Title          string
	Subtitle       string
	CurrentAccount string
	LastStatus     string
	UpdatedAt      time.Time
	Logs           []string
	LogOffset      int
}

// runTUIModel 描述配置页、worker 卡片列表与底部统计栏的完整状态。
// Why: 现在 TUI 同时承担“配置入口”和“运行期监控”两种职责，统一模型可以保证界面切换时状态连续。
type runTUIModel struct {
	baseConfig  config
	ready       chan struct{}
	startCh     chan<- config
	statsClient func(config) *webMailClient

	phase      tuiPhase
	started    bool
	running    bool
	finished   bool
	finishedAt time.Time

	modeIndex       int
	focusIndex      int
	homeActionIndex int
	configError     string
	configNotice    string

	webMailURLInput       textinput.Model
	emailInput            textinput.Model
	passwordInput         textinput.Model
	userFileInput         textinput.Model
	authDirInput          textinput.Model
	accountsFileInput     textinput.Model
	cpaURLInput           textinput.Model
	cpaKeyInput           textinput.Model
	proxyInput            textinput.Model
	mailboxInput          textinput.Model
	countInput            textinput.Model
	workersInput          textinput.Model
	authorizeWorkersInput textinput.Model
	timeoutInput          textinput.Model
	otpTimeoutInput       textinput.Model
	pollIntervalInput     textinput.Model
	requestTimeoutInput   textinput.Model

	viewport      viewport.Model
	logCards      map[string]*tuiLogCard
	cardOrder     []string
	focusedCardID string

	width  int
	height int

	registerSuccess  int
	registerFail     int
	authorizeSuccess int
	authorizeFail    int

	registerStartedAt  time.Time
	registerFinishedAt time.Time

	emailPoolStats       emailPoolStats
	emailPoolStatsLoaded bool
	emailPoolStatsError  string
}

func newRunTUIModel(cfg config, ready chan struct{}, startCh chan<- config, loadErr error) *runTUIModel {
	logViewport := viewport.New(0, 0)
	logViewport.MouseWheelEnabled = true

	model := &runTUIModel{
		baseConfig: cfg,
		ready:      ready,
		startCh:    startCh,
		statsClient: func(currentCfg config) *webMailClient {
			return newWebMailClient(currentCfg.webMailURL, currentCfg.requestTimeout)
		},
		phase:                 tuiPhaseHome,
		modeIndex:             tuiModeIndex(cfg.mode),
		focusIndex:            tuiFocusMode,
		homeActionIndex:       tuiHomeActionStart,
		webMailURLInput:       newTUIStringInput(cfg.webMailURL, false),
		emailInput:            newTUIStringInput(cfg.email, false),
		passwordInput:         newTUIStringInput(cfg.password, true),
		userFileInput:         newTUIStringInput(cfg.userFile, false),
		authDirInput:          newTUIStringInput(cfg.authDir, false),
		accountsFileInput:     newTUIStringInput(cfg.accountsFile, false),
		cpaURLInput:           newTUIStringInput(cfg.cpaURL, false),
		cpaKeyInput:           newTUIStringInput(cfg.cpaKey, true),
		proxyInput:            newTUIStringInput(cfg.proxy, false),
		mailboxInput:          newTUIStringInput(cfg.mailbox, false),
		countInput:            newTUIIntegerInput(cfg.count),
		workersInput:          newTUIIntegerInput(cfg.workers),
		authorizeWorkersInput: newTUIIntegerInput(cfg.authorizeWorkers),
		timeoutInput:          newTUIStringInput(cfg.overallTimeout.String(), false),
		otpTimeoutInput:       newTUIStringInput(cfg.otpTimeout.String(), false),
		pollIntervalInput:     newTUIStringInput(cfg.pollInterval.String(), false),
		requestTimeoutInput:   newTUIStringInput(cfg.requestTimeout.String(), false),
		viewport:              logViewport,
		logCards:              make(map[string]*tuiLogCard),
		cardOrder:             []string{},
	}
	if loadErr != nil {
		model.configError = fmt.Sprintf("读取 %s 失败: %v", tuiConfigFile, loadErr)
	}
	model.rebuildPreviewCards()
	return model
}

func (model *runTUIModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			close(model.ready)
			return nil
		},
		model.fetchEmailPoolStatsCmd(model.baseConfig),
		model.emailPoolStatsTickCmd(),
	)
}

func (model *runTUIModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		model.width = msg.Width
		model.height = msg.Height
		if model.phase != tuiPhaseConfig {
			model.syncViewportLayout()
		}
		return model, nil
	case tuiLogLineMsg:
		model.appendLog(msg.Line)
		return model, nil
	case tuiRegisterStartMsg:
		if model.registerStartedAt.IsZero() {
			model.registerStartedAt = msg.StartedAt
		}
		return model, nil
	case tuiRegisterFinishMsg:
		if model.registerStartedAt.IsZero() {
			model.registerStartedAt = msg.FinishedAt
		}
		model.registerFinishedAt = msg.FinishedAt
		if msg.Success {
			model.registerSuccess++
		} else {
			model.registerFail++
		}
		model.syncViewportLayout()
		return model, nil
	case tuiAuthorizeFinishMsg:
		if msg.Success {
			model.authorizeSuccess++
		} else {
			model.authorizeFail++
		}
		model.syncViewportLayout()
		return model, nil
	case tuiEmailPoolStatsMsg:
		if msg.Err != nil {
			model.emailPoolStatsLoaded = false
			model.emailPoolStatsError = msg.Err.Error()
		} else {
			model.emailPoolStats = msg.Stats
			model.emailPoolStatsLoaded = true
			model.emailPoolStatsError = ""
		}
		model.syncViewportLayout()
		return model, nil
	case tuiEmailPoolStatsTickMsg:
		return model, tea.Batch(
			model.fetchEmailPoolStatsCmd(model.baseConfig),
			model.emailPoolStatsTickCmd(),
		)
	case tuiFinishedMsg:
		model.running = false
		model.finished = true
		model.finishedAt = time.Now()
		model.syncViewportLayout()
		return model, nil
	}

	if model.phase == tuiPhaseConfig {
		return model, model.updateConfigPhase(message)
	}

	return model, model.updateHomePhase(message)
}

func (model *runTUIModel) View() string {
	if model.width == 0 || model.height == 0 {
		return "初始化 TUI..."
	}

	if model.phase == tuiPhaseConfig {
		return model.configView()
	}
	return model.homeView()
}

// homeView 只负责渲染首页日志区与底部统计栏。
// Why: 任务卡片已经被移除，首页现在回归成单一监控视图；配置页仍然独立，避免运行态和表单态混杂。
func (model *runTUIModel) homeView() string {
	return lipgloss.JoinVertical(lipgloss.Left, model.homeToolbarView(), model.viewport.View(), model.footerView())
}

// updateHomePhase 处理首页日志滚动和启动行为。
// Why: 去掉任务卡片后，首页只保留日志浏览，因此按键语义也应收敛到“滚日志”和“进入配置”。
func (model *runTUIModel) updateHomePhase(message tea.Msg) tea.Cmd {
	if keyMsg, ok := message.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "ctrl+c":
			return tea.Quit
		case "tab", "right":
			if !model.running {
				model.shiftHomeActionFocus(1)
			}
			return nil
		case "shift+tab", "left":
			if !model.running {
				model.shiftHomeActionFocus(-1)
			}
			return nil
		case "up", "k":
			model.scrollFocusedCard(1)
			model.syncViewportContent()
			return nil
		case "down", "j":
			model.scrollFocusedCard(-1)
			model.syncViewportContent()
			return nil
		case "enter":
			if model.running {
				return nil
			}
			switch model.homeActionIndex {
			case tuiHomeActionConfig:
				return model.switchToConfigPage()
			default:
				return model.startRun()
			}
		case "ctrl+s":
			if model.running {
				return nil
			}
			return model.startRun()
		}
	}

	var command tea.Cmd
	model.viewport, command = model.viewport.Update(message)
	return command
}

// homeToolbarView 渲染首页固定操作区，集中展示当前配置摘要和开始按钮。
// Why: 用户要求把开始按钮放到首页，因此首页必须有一个固定操作区，而不是把启动操作藏在配置页里。
func (model *runTUIModel) homeToolbarView() string {
	rows := []string{
		tuiTitleStyle.Render("Worker 首页 · 当前模式：" + displayRunMode(model.currentMode())),
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			model.renderHomeStartButton(),
			"  ",
			model.renderHomeConfigButton(),
			"  ",
			tuiMutedStyle.Render(homeActionHint(model.running)),
		),
	}

	style := tuiCardStyle
	if cardWidth := model.toolbarCardWidth(); cardWidth > 0 {
		style = style.Width(cardWidth)
	}
	return lipgloss.NewStyle().Padding(0, 1, 0, 1).Render(style.Render(strings.Join(rows, "\n")))
}

// updateConfigPhase 处理独立配置页表单的导航与提交。
// Why: 虽然配置页已经和首页分离，但配置项本身仍应保持单屏编辑体验，避免引入多步向导打断操作节奏。
func (model *runTUIModel) updateConfigPhase(message tea.Msg) tea.Cmd {
	switch msg := message.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return tea.Quit
		case "tab", "down":
			model.configNotice = ""
			return model.setFocus(model.focusIndex + 1)
		case "shift+tab":
			model.configNotice = ""
			if model.focusIndex == tuiFocusMode {
				return model.switchToHomePage()
			}
			return model.setFocus(model.focusIndex - 1)
		case "up":
			model.configNotice = ""
			return model.setFocus(model.focusIndex - 1)
		case "esc":
			return model.switchToHomePage()
		case "left":
			model.configNotice = ""
			if model.focusIndex == tuiFocusMode {
				model.shiftMode(-1)
			}
			return nil
		case "right", " ":
			model.configNotice = ""
			if model.focusIndex == tuiFocusMode {
				model.shiftMode(1)
			}
			return nil
		case "enter":
			switch model.focusIndex {
			case tuiFocusMode:
				model.configNotice = ""
				model.shiftMode(1)
				return nil
			case tuiFocusSave:
				return model.saveCurrentConfig()
			default:
				model.configNotice = ""
				return model.setFocus(model.focusIndex + 1)
			}
		case "ctrl+s":
			return model.saveCurrentConfig()
		}
	}

	input := model.activeInput()
	if input == nil {
		return nil
	}

	model.configNotice = ""
	var cmd tea.Cmd
	*input, cmd = input.Update(message)
	return cmd
}

func (model *runTUIModel) configView() string {
	currentMode := model.currentMode()
	modeHelp := tuiMutedStyle.Render("左右键切换运行模式；Tab 切下一个字段；首字段 Shift+Tab 返回首页；Ctrl+S 保存配置")
	modeUsageHint := tuiMutedStyle.Render(displayModeConfigHint(currentMode))
	persistHint := tuiMutedStyle.Render("保存配置只落盘，不会启动任务；开始运行请回到首页按 Enter 或 Ctrl+S")

	rows := []string{
		tuiTitleStyle.Render("运行配置"),
		model.renderConfigRow(tuiFocusMode, "运行模式", model.renderModeSelector()),
		lipgloss.NewStyle().PaddingLeft(16).Render(modeHelp),
		lipgloss.NewStyle().PaddingLeft(16).Render(modeUsageHint),
		model.renderConfigRow(tuiFocusWebMailURL, "邮箱服务地址", model.webMailURLInput.View()),
		model.renderConfigRow(tuiFocusEmail, "登录邮箱", renderConfigValueWithHint(model.emailInput.View(), loginCredentialFieldHint(currentMode, "email"))),
		model.renderConfigRow(tuiFocusPassword, "登录密码", renderConfigValueWithHint(model.passwordInput.View(), loginCredentialFieldHint(currentMode, "password"))),
		model.renderConfigRow(tuiFocusUserFile, "账号文件", renderConfigValueWithHint(model.userFileInput.View(), loginCredentialFieldHint(currentMode, "user-file"))),
		model.renderConfigRow(tuiFocusAuthDir, "授权目录", model.authDirInput.View()),
		model.renderConfigRow(tuiFocusAccountsFile, "状态文件", model.accountsFileInput.View()),
		model.renderConfigRow(tuiFocusCPAURL, "CPA 地址", model.cpaURLInput.View()),
		model.renderConfigRow(tuiFocusCPAKey, "CPA 密钥", model.cpaKeyInput.View()),
		model.renderConfigRow(tuiFocusProxy, "代理地址", model.proxyInput.View()),
		model.renderConfigRow(tuiFocusMailbox, "邮箱目录", model.mailboxInput.View()),
		model.renderConfigRow(tuiFocusCount, "注册数量", model.countInput.View()+" "+tuiMutedStyle.Render("仅注册 / 注册+授权 生效")),
		model.renderConfigRow(tuiFocusWorkers, "并发数量", model.workersInput.View()+" "+tuiMutedStyle.Render("仅注册 / 仅授权 生效")),
		model.renderConfigRow(tuiFocusAuthorizeWorkers, "授权并发", model.authorizeWorkersInput.View()+" "+tuiMutedStyle.Render("兼容旧配置保留；当前单链路模式已忽略")),
		model.renderConfigRow(tuiFocusTimeout, "整体超时", model.timeoutInput.View()),
		model.renderConfigRow(tuiFocusOTPTimeout, "验证码超时", model.otpTimeoutInput.View()),
		model.renderConfigRow(tuiFocusPollInterval, "轮询间隔", model.pollIntervalInput.View()),
		model.renderConfigRow(tuiFocusRequestTimeout, "请求超时", model.requestTimeoutInput.View()),
		model.renderSaveButton(),
		persistHint,
	}

	if model.configError != "" {
		rows = append(rows, tuiFailStyle.Render("配置错误: "+model.configError))
	}
	if model.configNotice != "" {
		rows = append(rows, tuiSuccessStyle.Render(model.configNotice))
	}

	style := tuiCardStyle
	if cardWidth := model.configCardWidth(); cardWidth > 0 {
		style = style.Width(cardWidth)
	}
	content := style.Render(strings.Join(rows, "\n"))
	return lipgloss.NewStyle().Padding(1, 1).Render(content)
}

func (model *runTUIModel) renderConfigRow(index int, label string, value string) string {
	labelStyle := tuiFieldLabelStyle
	if model.focusIndex == index {
		labelStyle = tuiFocusedLabelStyle
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, labelStyle.Render(label), value)
}

func (model *runTUIModel) renderModeSelector() string {
	options := make([]string, 0, len(tuiModeOptions))
	for index, mode := range tuiModeOptions {
		style := tuiUnselectedModeStyle
		if index == model.modeIndex {
			style = tuiSelectedModeStyle
		}
		options = append(options, style.Render(displayRunMode(mode)))
	}
	return strings.Join(options, " ")
}

func (model *runTUIModel) renderSaveButton() string {
	if model.focusIndex == tuiFocusSave {
		return tuiStartButtonStyle.Render("保存配置")
	}
	return tuiStartIdleStyle.Render("保存配置")
}

// renderHomeStartButton 渲染首页的开始按钮。
// Why: 启动动作已经从配置页移到首页，因此首页必须明确展示当前是否可启动，而不能只靠底栏提示。
func (model *runTUIModel) renderHomeStartButton() string {
	if !model.running && model.homeActionIndex == tuiHomeActionStart {
		if model.finished {
			return tuiStartButtonStyle.Render("再次开始")
		}
		return tuiStartButtonStyle.Render("开始运行")
	}
	if model.running {
		return tuiStartIdleStyle.Render("运行中")
	}
	if model.finished {
		return tuiStartIdleStyle.Render("再次开始")
	}
	return tuiStartIdleStyle.Render("开始运行")
}

// renderHomeConfigButton 渲染首页的系统配置按钮。
// Why: 用户要求配置入口通过首页按钮进入，而不是依赖单独快捷键，因此这里提供显式操作入口。
func (model *runTUIModel) renderHomeConfigButton() string {
	if model.running {
		return tuiStartIdleStyle.Render("系统配置")
	}
	if model.homeActionIndex == tuiHomeActionConfig {
		return tuiStartButtonStyle.Render("系统配置")
	}
	return tuiStartIdleStyle.Render("系统配置")
}

// homeActionHint 返回首页操作区旁边的辅助提示。
// Why: 首页不再承担参数编辑职责，因此需要用一句短提示把“开始”和“去配置页”的入口同时讲清楚。
func homeActionHint(running bool) string {
	if running {
		return "上下滚日志，PgUp/PgDn 滚整页"
	}
	return "Tab/左右切换按钮，Enter 执行，上下滚日志"
}

// switchToConfigPage 负责从首页进入配置页，并把光标落到首个可编辑字段。
// Why: 配置页现在是独立入口，进入时必须显式恢复输入焦点，否则用户会看到表单却无法直接编辑。
func (model *runTUIModel) switchToConfigPage() tea.Cmd {
	model.phase = tuiPhaseConfig
	model.focusIndex = tuiFocusMode
	return model.setFocus(tuiFocusMode)
}

// switchToHomePage 负责从配置页返回首页，并在非运行态下刷新占位卡片。
// Why: 首页既要支持首次启动前预览，也要支持任务结束后继续修改下一轮配置，因此回首页时需要按当前表单重建下一轮预览。
func (model *runTUIModel) switchToHomePage() tea.Cmd {
	model.phase = tuiPhaseHome
	model.homeActionIndex = tuiHomeActionStart
	for _, input := range model.allInputs() {
		input.Blur()
	}
	if !model.running {
		model.rebuildPreviewCards()
		model.syncViewportLayout()
	}
	return nil
}

// currentMode 返回配置页当前选中的运行模式。
// Why: 配置页多个提示文案都依赖当前 mode，集中从一个函数取值可以避免界面和实际启动配置出现偏差。
func (model *runTUIModel) currentMode() runMode {
	return tuiModeOptions[model.modeIndex]
}

// currentPreviewWorkers 返回首页摘要和占位卡片应使用的注册 worker 数量。
// Why: 首页和配置页同时读同一组表单值，必须以宽松解析方式共享同一份“当前预览”结果，避免展示和实际启动脱节。
func (model *runTUIModel) currentPreviewWorkers() int {
	return parsePreviewPositiveInt(model.workersInput.Value(), model.baseConfig.workers)
}

// currentPreviewAuthorizeWorkers 返回首页摘要和占位卡片应使用的授权 worker 数量。
// Why: pipeline/authorize 模式都依赖这组数字做首页布局预览，因此需要和 currentPreviewWorkers 一样统一从表单值解析。
func (model *runTUIModel) currentPreviewAuthorizeWorkers() int {
	return parsePreviewPositiveInt(model.authorizeWorkersInput.Value(), model.baseConfig.authorizeWorkers)
}

func (model *runTUIModel) setFocus(next int) tea.Cmd {
	if next < 0 {
		next = tuiFocusTotal - 1
	}
	model.focusIndex = next % tuiFocusTotal

	for _, input := range model.allInputs() {
		input.Blur()
	}

	input := model.activeInput()
	if input == nil {
		return nil
	}
	return input.Focus()
}

func (model *runTUIModel) shiftMode(delta int) {
	size := len(tuiModeOptions)
	model.modeIndex = (model.modeIndex + delta + size) % size
}

func (model *runTUIModel) allInputs() []*textinput.Model {
	return []*textinput.Model{
		&model.webMailURLInput,
		&model.emailInput,
		&model.passwordInput,
		&model.userFileInput,
		&model.authDirInput,
		&model.accountsFileInput,
		&model.cpaURLInput,
		&model.cpaKeyInput,
		&model.proxyInput,
		&model.mailboxInput,
		&model.countInput,
		&model.workersInput,
		&model.authorizeWorkersInput,
		&model.timeoutInput,
		&model.otpTimeoutInput,
		&model.pollIntervalInput,
		&model.requestTimeoutInput,
	}
}

func (model *runTUIModel) activeInput() *textinput.Model {
	switch model.focusIndex {
	case tuiFocusWebMailURL:
		return &model.webMailURLInput
	case tuiFocusEmail:
		return &model.emailInput
	case tuiFocusPassword:
		return &model.passwordInput
	case tuiFocusUserFile:
		return &model.userFileInput
	case tuiFocusAuthDir:
		return &model.authDirInput
	case tuiFocusAccountsFile:
		return &model.accountsFileInput
	case tuiFocusCPAURL:
		return &model.cpaURLInput
	case tuiFocusCPAKey:
		return &model.cpaKeyInput
	case tuiFocusProxy:
		return &model.proxyInput
	case tuiFocusMailbox:
		return &model.mailboxInput
	case tuiFocusCount:
		return &model.countInput
	case tuiFocusWorkers:
		return &model.workersInput
	case tuiFocusAuthorizeWorkers:
		return &model.authorizeWorkersInput
	case tuiFocusTimeout:
		return &model.timeoutInput
	case tuiFocusOTPTimeout:
		return &model.otpTimeoutInput
	case tuiFocusPollInterval:
		return &model.pollIntervalInput
	case tuiFocusRequestTimeout:
		return &model.requestTimeoutInput
	default:
		return nil
	}
}

// buildCurrentRunConfig 基于当前表单值构建可运行配置。
// Why: 首页开始按钮和配置页保存按钮都会消费同一套表单数据，因此构建逻辑必须收敛到一个函数，避免双份校验。
func (model *runTUIModel) buildCurrentRunConfig() (config, error) {
	return buildTUIRunConfig(
		model.baseConfig,
		tuiModeOptions[model.modeIndex],
		model.webMailURLInput.Value(),
		model.emailInput.Value(),
		model.passwordInput.Value(),
		model.userFileInput.Value(),
		model.authDirInput.Value(),
		model.accountsFileInput.Value(),
		model.cpaURLInput.Value(),
		model.cpaKeyInput.Value(),
		model.proxyInput.Value(),
		model.mailboxInput.Value(),
		model.countInput.Value(),
		model.workersInput.Value(),
		model.authorizeWorkersInput.Value(),
		model.timeoutInput.Value(),
		model.otpTimeoutInput.Value(),
		model.pollIntervalInput.Value(),
		model.requestTimeoutInput.Value(),
	)
}

// saveCurrentConfig 只保存当前配置，不启动任务。
// Why: 用户要求配置页提供独立保存按钮，因此这里要显式区分“落盘配置”和“启动任务”两个动作。
func (model *runTUIModel) saveCurrentConfig() tea.Cmd {
	runCfg, err := model.buildCurrentRunConfig()
	if err != nil {
		model.configError = err.Error()
		model.configNotice = ""
		return nil
	}
	if err := savePersistedTUIConfig(runCfg); err != nil {
		model.configError = fmt.Sprintf("保存 %s 失败: %v", tuiConfigFile, err)
		model.configNotice = ""
		return nil
	}

	model.baseConfig = runCfg
	model.configError = ""
	model.configNotice = "配置已保存到 .config.json"
	return nil
}

// startRun 会把当前表单值保存为真实配置，并切回首页开始渲染运行状态。
// Why: 开始按钮现在位于首页，因此启动前既要吃到配置页刚保存的值，也要兼容用户直接从首页启动当前配置。
func (model *runTUIModel) startRun() tea.Cmd {
	runCfg, err := model.buildCurrentRunConfig()
	if err != nil {
		model.configError = err.Error()
		model.configNotice = ""
		if model.phase == tuiPhaseHome {
			return model.switchToConfigPage()
		}
		return nil
	}
	if err := savePersistedTUIConfig(runCfg); err != nil {
		model.configError = fmt.Sprintf("保存 %s 失败: %v", tuiConfigFile, err)
		model.configNotice = ""
		if model.phase == tuiPhaseHome {
			return model.switchToConfigPage()
		}
		return nil
	}

	model.baseConfig = runCfg
	model.phase = tuiPhaseHome
	model.started = true
	model.running = true
	model.finished = false
	model.finishedAt = time.Time{}
	model.configError = ""
	model.configNotice = ""
	model.resetRunMetrics()
	model.resetRunningCards()
	model.seedWorkerCardsForConfig(runCfg)
	model.appendLog(fmt.Sprintf(
		"启动配置: 运行模式=%s 注册数量=%d 并发数量=%d 授权并发=%d 代理地址=%s",
		displayRunMode(runCfg.mode),
		runCfg.count,
		runCfg.workers,
		runCfg.authorizeWorkers,
		runCfg.proxy,
	))
	model.syncViewportLayout()

	select {
	case model.startCh <- runCfg:
	default:
	}
	return model.fetchEmailPoolStatsCmd(runCfg)
}

// resetRunMetrics 在每次重新启动前清空统计，避免多轮任务的成功/失败数相互叠加。
// Why: 用户现在可以在同一进程内多次启动任务，如果不清空统计，首页底栏会混入上一轮结果。
func (model *runTUIModel) resetRunMetrics() {
	model.registerSuccess = 0
	model.registerFail = 0
	model.authorizeSuccess = 0
	model.authorizeFail = 0
	model.registerStartedAt = time.Time{}
	model.registerFinishedAt = time.Time{}
}

// appendLog 把单条日志追加到统一系统日志视图。
// Why: 任务卡片已经被移除，但日志内容里仍保留 worker 前缀，既能节省界面高度，也不会丢失来源信息。
func (model *runTUIModel) appendLog(line string) {
	cardID, title, subtitle, displayLine := classifyTUILogLine(line)
	card := model.ensureLogCard(cardID, title, subtitle)
	if subtitle != "" && card.Kind == "system" {
		card.Subtitle = subtitle
	}
	card.LastStatus = inferWorkerStatus(displayLine, card.LastStatus)
	card.UpdatedAt = time.Now()
	card.Logs = append(card.Logs, displayLine)
	if len(card.Logs) > maxTUILogLines {
		card.Logs = card.Logs[len(card.Logs)-maxTUILogLines:]
	}
	model.syncViewportContent()
}

func (model *runTUIModel) syncViewportContent() {
	wasAtBottom := model.viewport.TotalLineCount() == 0 || model.viewport.AtBottom()
	model.viewport.SetContent(model.renderRunningCards())
	if wasAtBottom {
		model.viewport.GotoBottom()
	}
}

// syncViewportLayout 根据当前窗口尺寸为日志区预留固定底栏空间。
// Why: 统计栏高度会随终端宽度变化，因此日志 viewport 的高度必须在每次 resize 和统计更新后重新计算。
func (model *runTUIModel) syncViewportLayout() {
	if model.width <= 0 || model.height <= 0 {
		return
	}

	footerHeight := lipgloss.Height(model.footerView())
	headerHeight := 0
	if model.phase == tuiPhaseHome {
		headerHeight = lipgloss.Height(model.homeToolbarView())
	}
	logHeight := model.height - footerHeight - headerHeight
	if logHeight < 1 {
		logHeight = 1
	}

	model.viewport.Width = model.viewportContentWidth()
	model.viewport.Height = logHeight
	model.syncViewportContent()
}

func (model *runTUIModel) resetRunningCards() {
	model.logCards = make(map[string]*tuiLogCard)
	model.cardOrder = model.cardOrder[:0]
	model.focusedCardID = ""
	model.ensureLogCard(tuiSystemCardID, "系统日志", "汇总主流程与未绑定 worker 的输出")
}

// rebuildPreviewCards 在空闲态下重置首页日志视图。
// Why: 首页不再展示 worker 占位卡片，但切回首页时仍要清理上一轮运行期残留的瞬时 UI 状态。
func (model *runTUIModel) rebuildPreviewCards() {
	model.resetRunningCards()
	model.syncViewportContent()
}

// seedWorkerCardsForConfig 根据模式和并发数预建空卡片，占住首页布局。
// Why: 用户希望首页固定看到 worker 卡片列表，即便任务尚未开始，也要提前展示每个 worker 的占位状态。
func (model *runTUIModel) seedWorkerCardsForConfig(cfg config) {
	registerWorkers, authorizeWorkers := workerCardLayoutForMode(cfg.mode, cfg.workers, cfg.authorizeWorkers)
	for workerID := 1; workerID <= registerWorkers; workerID++ {
		index := strconv.Itoa(workerID)
		card := model.ensureLogCard("worker-"+index, displayWorkerCardTitle("worker", index), "")
		card.LastStatus = "等待启动"
	}
	for workerID := 1; workerID <= authorizeWorkers; workerID++ {
		index := strconv.Itoa(workerID)
		card := model.ensureLogCard("auth-"+index, displayWorkerCardTitle("auth", index), "")
		card.LastStatus = "等待启动"
	}
}

func (model *runTUIModel) ensureLogCard(cardID, title, subtitle string) *tuiLogCard {
	if card, ok := model.logCards[cardID]; ok {
		if title != "" {
			card.Title = title
		}
		if subtitle != "" {
			card.Subtitle = subtitle
		}
		return card
	}

	kind, workerIndex := parseLogCardIdentity(cardID)
	card := &tuiLogCard{
		ID:          cardID,
		Kind:        kind,
		WorkerIndex: workerIndex,
		Title:       title,
		Subtitle:    subtitle,
		Logs:        []string{},
	}
	model.logCards[cardID] = card
	model.cardOrder = append(model.cardOrder, cardID)
	if model.focusedCardID == "" && kind != "system" {
		model.focusedCardID = cardID
	}
	return card
}

func (model *runTUIModel) renderRunningCards() string {
	systemCard, ok := model.logCards[tuiSystemCardID]
	if !ok || systemCard == nil {
		return tuiMutedStyle.Render("等待日志...")
	}
	model.ensureFocusedLogCard()
	return model.renderLogCard(systemCard, model.fullCardWidth(), model.isFocusedCard(systemCard.ID))
}

func (model *runTUIModel) renderTwoColumnCards(registerCards []*tuiLogCard, authorizeCards []*tuiLogCard) string {
	if len(registerCards) == 0 && len(authorizeCards) == 0 {
		return ""
	}

	leftCards := make([]string, 0, len(registerCards))
	rightCards := make([]string, 0, len(authorizeCards))
	columnWidth := model.columnCardWidth()

	for _, card := range registerCards {
		leftCards = append(leftCards, model.renderLogCard(card, columnWidth, model.isFocusedCard(card.ID)))
	}
	for _, card := range authorizeCards {
		rightCards = append(rightCards, model.renderLogCard(card, columnWidth, model.isFocusedCard(card.ID)))
	}

	leftColumn := strings.Join(leftCards, strings.Repeat("\n", tuiRunningCardGap+1))
	rightColumn := strings.Join(rightCards, strings.Repeat("\n", tuiRunningCardGap+1))

	if leftColumn == "" {
		leftColumn = tuiMutedStyle.Width(columnWidth).Render("暂无注册 Worker")
	}
	if rightColumn == "" {
		rightColumn = tuiMutedStyle.Width(columnWidth).Render("暂无授权 Worker")
	}

	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		lipgloss.NewStyle().Width(columnWidth).Render(leftColumn),
		lipgloss.NewStyle().Width(tuiRunningCardGap).Render(""),
		lipgloss.NewStyle().Width(columnWidth).Render(rightColumn),
	)
}

func (model *runTUIModel) renderLogCard(card *tuiLogCard, cardWidth int, focused bool) string {
	rows := []string{tuiCardTitleStyle.Render(card.Title)}

	switch card.Kind {
	case "system":
		if card.Subtitle != "" {
			rows = append(rows, tuiCardSubtitleStyle.Render(card.Subtitle))
		}
	default:
		account := card.CurrentAccount
		if account == "" {
			account = "--"
		}
		status := card.LastStatus
		if status == "" {
			status = "等待中"
		}
		updatedAt := "--"
		if !card.UpdatedAt.IsZero() {
			updatedAt = card.UpdatedAt.Format("15:04:05")
		}
		rows = append(rows, tuiCardSubtitleStyle.Render("当前账号: "+account))
		rows = append(rows, tuiCardSubtitleStyle.Render("最后状态: "+status+"    更新时间: "+updatedAt))
	}

	rows = append(rows, formatWorkerCardLogRows(card.Logs, model.cardVisibleLogLines(card), model.cardLogWidth(card.Kind, cardWidth), card.LogOffset)...)

	style := tuiLogCardStyle
	if focused {
		style = tuiFocusedLogCardStyle
	}
	if cardWidth > 0 {
		style = style.Width(cardWidth)
	}
	return style.Render(strings.Join(rows, "\n"))
}

func (model *runTUIModel) fullCardWidth() int {
	if model.width <= 0 {
		return 0
	}
	cardWidth := model.viewportContentWidth()
	if cardWidth < 20 {
		return 20
	}
	return cardWidth
}

func (model *runTUIModel) columnCardWidth() int {
	fullWidth := model.fullCardWidth()
	columnWidth := (fullWidth - tuiRunningCardGap) / 2
	if columnWidth < 20 {
		return 20
	}
	return columnWidth
}

// cardVisibleLogLines 返回不同卡片类型的可见日志行数。
// Why: 系统日志现在独占首页主体区域，应尽量把 viewport 的可用高度都让给日志本身；
// worker 卡片仍保持固定窗口，避免未来恢复多卡片布局时单张卡片无限增高。
func (model *runTUIModel) cardVisibleLogLines(card *tuiLogCard) int {
	if card != nil && card.Kind == "system" {
		return model.systemCardVisibleLogLines(card)
	}
	switch {
	case model.height >= 42:
		return 5
	case model.height >= 32:
		return 4
	default:
		return 3
	}
}

// systemCardVisibleLogLines 计算系统日志卡片的动态可见行数。
// Why: 用户要求系统日志直接占满首页，因此这里按首页 viewport 的实时高度反推日志窗口行数，
// 而不是继续使用固定的 5~8 行上限。
func (model *runTUIModel) systemCardVisibleLogLines(card *tuiLogCard) int {
	if model.viewport.Height <= 0 {
		switch {
		case model.height >= 42:
			return 8
		case model.height >= 32:
			return 6
		default:
			return 5
		}
	}
	visible := model.viewport.Height - 1
	if card != nil && strings.TrimSpace(card.Subtitle) != "" {
		visible--
	}
	if visible < 1 {
		return 1
	}
	return visible
}

// cardLogWidth 返回卡片日志区可安全使用的单行宽度。
// Why: 不同卡片都有内部滚动时，日志必须统一做单行裁剪，避免任何一张卡片因为自动换行而撑高。
func (model *runTUIModel) cardLogWidth(kind string, cardWidth int) int {
	logWidth := cardWidth - 8
	if kind == "system" {
		logWidth = cardWidth - 6
	}
	if logWidth < 12 {
		return 12
	}
	return logWidth
}

// formatWorkerCardLogRows 把 worker 日志裁成固定高度的窗口，避免单张卡片持续增高破坏双列布局。
// Why: 用户反馈左右列会互相“拉扯”，本质是卡片高度随日志增长而变化，因此这里固定只显示最近几行。
func formatWorkerCardLogRows(logs []string, maxVisible int, lineWidth int, offset int) []string {
	if maxVisible <= 0 {
		return []string{}
	}

	rows := make([]string, 0, maxVisible)
	if len(logs) == 0 {
		rows = append(rows, tuiMutedStyle.Render("等待日志..."))
		for len(rows) < maxVisible {
			rows = append(rows, "")
		}
		return rows
	}

	maxOffset := maxInt(len(logs)-maxVisible, 0)
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	start := maxInt(len(logs)-maxVisible-offset, 0)
	end := minInt(start+maxVisible, len(logs))
	visible := logs[start:end]
	for index, line := range visible {
		prefix := "• "
		if index == 0 && start > 0 {
			prefix = "↑ "
		}
		if index == len(visible)-1 && end < len(logs) {
			prefix = "↓ "
		}
		rows = append(rows, clipCardLogLine(prefix, line, lineWidth))
	}
	for len(rows) < maxVisible {
		rows = append(rows, "")
	}
	return rows
}

// clipCardLogLine 截断单行日志，避免长文本在卡片内自动换行。
// Why: 只要日志换行，卡片高度就会失控增长，因此这里必须在渲染前主动裁剪。
func clipCardLogLine(prefix string, line string, maxWidth int) string {
	if maxWidth <= 0 {
		return prefix + line
	}

	text := strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
	available := maxWidth - runeCount(prefix)
	if available <= 1 {
		return prefix
	}

	runes := []rune(text)
	if len(runes) <= available {
		return prefix + text
	}
	if available == 1 {
		return prefix + "…"
	}
	return prefix + string(runes[:available-1]) + "…"
}

// runeCount 返回字符串的 rune 数量，避免中文日志被按字节截断。
// Why: TUI 日志是中文为主，按字节裁剪会把多字节字符切坏，导致显示异常。
func runeCount(value string) int {
	return len([]rune(value))
}

// ensureFocusedLogCard 确保首页始终有一个可聚焦的卡片。
// Why: 现在系统日志和 worker 卡片都支持内部滚动，数据刷新或模式切换后必须自动纠正焦点，避免按键落空。
func (model *runTUIModel) ensureFocusedLogCard() {
	if model.focusedCardID != "" {
		if _, ok := model.logCards[model.focusedCardID]; ok {
			return
		}
	}
	model.focusedCardID = ""
	for _, cardID := range model.cardOrder {
		if _, ok := model.logCards[cardID]; ok {
			model.focusedCardID = cardID
			return
		}
	}
}

// isFocusedCard 返回当前卡片是否为首页焦点卡片。
// Why: 焦点高亮是用户判断“上下键会滚动哪张卡片”的唯一视觉锚点，因此需要统一判断入口。
func (model *runTUIModel) isFocusedCard(cardID string) bool {
	return model.focusedCardID != "" && model.focusedCardID == cardID
}

// shiftFocusedCard 在首页的可聚焦卡片之间切换焦点。
// Why: 双列卡片需要独立内部滚动时，必须先提供一个稳定的卡片焦点移动机制。
func (model *runTUIModel) shiftFocusedCard(delta int) {
	focusable := model.focusableLogCardIDs()
	if len(focusable) == 0 {
		model.focusedCardID = ""
		return
	}

	currentIndex := 0
	for index, cardID := range focusable {
		if cardID == model.focusedCardID {
			currentIndex = index
			break
		}
	}
	currentIndex = (currentIndex + delta + len(focusable)) % len(focusable)
	model.focusedCardID = focusable[currentIndex]
}

// scrollFocusedCard 调整当前焦点卡片的日志偏移，实现卡片内部滚动。
// Why: 用户希望每张 worker 卡片独立滚动日志，而不是继续把整列布局顶乱，因此偏移状态必须挂在卡片本身。
func (model *runTUIModel) scrollFocusedCard(delta int) {
	model.ensureFocusedLogCard()
	if model.focusedCardID == "" {
		return
	}
	card, ok := model.logCards[model.focusedCardID]
	if !ok {
		return
	}

	maxOffset := maxInt(len(card.Logs)-model.cardVisibleLogLines(card), 0)
	card.LogOffset += delta
	if card.LogOffset < 0 {
		card.LogOffset = 0
	}
	if card.LogOffset > maxOffset {
		card.LogOffset = maxOffset
	}
}

// focusableLogCardIDs 返回首页所有可聚焦卡片 ID。
// Why: 系统日志卡片也支持内部滚动后，首页所有卡片都应该参与统一的焦点切换。
func (model *runTUIModel) focusableLogCardIDs() []string {
	ids := make([]string, 0, len(model.cardOrder))
	for _, cardID := range model.cardOrder {
		if _, ok := model.logCards[cardID]; ok {
			ids = append(ids, cardID)
		}
	}
	return ids
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// viewportContentWidth 返回日志区真正可安全渲染的内容宽度。
// Why: 终端视口右侧没有额外缓冲时，卡片总宽度一旦贴边就会把最后一列边框裁掉，因此这里统一预留一个安全边距。
func (model *runTUIModel) viewportContentWidth() int {
	if model.width <= 0 {
		return 0
	}
	contentWidth := model.width - 2
	if contentWidth < 20 {
		return 20
	}
	return contentWidth
}

// toolbarCardWidth 返回首页工具栏内容宽度。
// Why: 边框已移除，这里只保留最小安全宽度约束，避免标题和按钮在窄终端里过度压缩。
func (model *runTUIModel) toolbarCardWidth() int {
	contentWidth := model.viewportContentWidth()
	if contentWidth < 20 {
		return 20
	}
	return contentWidth
}

// configCardWidth 返回配置页主区域的内容宽度，并限制在可读范围内。
// Why: 去掉边框后仍需要控制配置页横向阅读宽度，避免超宽终端下表单过长影响扫描效率。
func (model *runTUIModel) configCardWidth() int {
	contentWidth := model.viewportContentWidth()
	if contentWidth > 104 {
		contentWidth = 104
	}
	if contentWidth < 20 {
		return 20
	}
	return contentWidth
}

func (model *runTUIModel) footerView() string {
	emailPoolLine := lipgloss.JoinHorizontal(
		lipgloss.Left,
		tuiMutedStyle.Render("邮箱池 "),
		tuiValueStyle.Render(model.emailPoolSummaryText()),
	)
	workerLine := lipgloss.JoinHorizontal(
		lipgloss.Left,
		tuiMutedStyle.Render("线程 "),
		tuiValueStyle.Render(model.workerSummaryText()),
		tuiMutedStyle.Render("   平均注册速度 "),
		tuiValueStyle.Render(formatAverageRegisterSpeed(model.registerStartedAt, model.registerFinishedAt, model.registerSuccess+model.registerFail)),
	)
	statsLine := lipgloss.JoinHorizontal(
		lipgloss.Left,
		renderStat("注册成功", model.registerSuccess, tuiSuccessStyle),
		renderStat("注册失败", model.registerFail, tuiFailStyle),
		renderStat("授权成功", model.authorizeSuccess, tuiSuccessStyle),
		renderStat("授权失败", model.authorizeFail, tuiFailStyle),
	)
	hintLine := model.footerHintLine()

	footer := lipgloss.JoinVertical(lipgloss.Left, statsLine, emailPoolLine, workerLine, hintLine)
	style := tuiFooterStyle
	if model.width > 0 {
		style = style.Width(model.width)
	}
	return style.Render(footer)
}

// footerHintLine 渲染底栏最后一行，把操作提示放左侧、版本号放右侧。
// Why: 用户需要在右下角稳定看到当前版本，因此这里显式为提示区预留右对齐版本位。
func (model *runTUIModel) footerHintLine() string {
	hintText := tuiMutedStyle.Render(model.footerHint())
	versionText := tuiMutedStyle.Render("版本 " + appVersion)
	if model.width <= 0 {
		return lipgloss.JoinHorizontal(lipgloss.Left, hintText, "   ", versionText)
	}

	innerWidth := model.width - 2
	if innerWidth <= lipgloss.Width(hintText)+lipgloss.Width(versionText)+1 {
		return lipgloss.JoinHorizontal(lipgloss.Left, hintText, "   ", versionText)
	}

	gapWidth := innerWidth - lipgloss.Width(hintText) - lipgloss.Width(versionText)
	if gapWidth < 1 {
		gapWidth = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, hintText, strings.Repeat(" ", gapWidth), versionText)
}

// emailPoolSummaryText 返回底部状态栏的邮箱池摘要。
// Why: 非 webmail 模式启动时需要快速看到“可用 / 已使用 / 未使用 / 租用中”，避免用户还要额外手查 stats 接口。
func (model *runTUIModel) emailPoolSummaryText() string {
	if model.emailPoolStatsLoaded {
		unused := model.emailPoolStats.Total - model.emailPoolStats.Used
		if unused < 0 {
			unused = 0
		}
		return fmt.Sprintf(
			"可用=%d，已使用=%d，未使用=%d，租用中=%d",
			model.emailPoolStats.Available,
			model.emailPoolStats.Used,
			unused,
			model.emailPoolStats.Leased,
		)
	}
	if model.emailPoolStatsError != "" {
		return "获取失败"
	}
	return "读取中..."
}

// fetchEmailPoolStatsCmd 异步拉取当前邮箱池统计。
// Why: stats 接口依赖本地 web_mail 服务，必须异步请求，避免 TUI 初始化或开始按钮阻塞界面。
func (model *runTUIModel) fetchEmailPoolStatsCmd(cfg config) tea.Cmd {
	if model == nil || model.statsClient == nil || cfg.mode == modeWebMail || strings.TrimSpace(cfg.webMailURL) == "" {
		return nil
	}

	return func() tea.Msg {
		client := model.statsClient(cfg)
		if client == nil {
			return tuiEmailPoolStatsMsg{}
		}

		ctx, cancel := context.WithTimeout(context.Background(), cfg.requestTimeout)
		defer cancel()

		stats, err := client.getStats(ctx)
		return tuiEmailPoolStatsMsg{
			Stats: stats,
			Err:   err,
		}
	}
}

// emailPoolStatsTickCmd 周期性触发邮箱池统计刷新。
// Why: 启动后邮箱池状态会持续变化，只在初始化和开始运行时拉一次会很快过期，因此需要低频自动刷新。
func (model *runTUIModel) emailPoolStatsTickCmd() tea.Cmd {
	if model == nil {
		return nil
	}
	return tea.Tick(tuiEmailPoolStatsRefreshInterval, func(time.Time) tea.Msg {
		return tuiEmailPoolStatsTickMsg{}
	})
}

// workerSummaryText 返回底部状态栏里展示的 worker 数量摘要。
// Why: 用户希望把线程数量从顶部摘要挪到底栏，启动前预览和运行中状态都应使用同一套文案。
func (model *runTUIModel) workerSummaryText() string {
	registerWorkers, authorizeWorkers := workerCardLayoutForMode(model.currentMode(), model.currentPreviewWorkers(), model.currentPreviewAuthorizeWorkers())
	return fmt.Sprintf("注册线程=%d，授权线程=%d", registerWorkers, authorizeWorkers)
}

// footerHint 统一返回首页底栏提示，避免首页和运行态分别维护两套快捷键文案。
// Why: 首页已经不再展示配置表单，用户只能从底栏感知“如何进入配置页”以及“当前是否已启动”。
func (model *runTUIModel) footerHint() string {
	if model.finished {
		return "任务已结束：Tab/左右切换按钮，Enter 执行，上下滚日志。"
	}
	if model.running {
		return "运行中：上下滚日志，PgUp/PgDn 滚整页，Ctrl+C 退出。"
	}
	return "首页预览：Tab/左右切换按钮，Enter 执行，上下滚日志。"
}

// shiftHomeActionFocus 切换首页顶部按钮焦点。
// Why: 去掉配置快捷键后，首页按钮焦点成为唯一的配置入口，因此需要稳定的按钮切换行为。
func (model *runTUIModel) shiftHomeActionFocus(delta int) {
	model.homeActionIndex = (model.homeActionIndex + delta + tuiHomeActionTotal) % tuiHomeActionTotal
}

func renderStat(label string, value int, valueStyle lipgloss.Style) string {
	return lipgloss.JoinHorizontal(
		lipgloss.Left,
		tuiMutedStyle.Render(label+" "),
		valueStyle.Render(fmt.Sprintf("%d", value)),
		tuiMutedStyle.Render("   "),
	)
}

func classifyTUILogLine(line string) (cardID string, title string, subtitle string, displayLine string) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(line, "[go-register] "))
	return tuiSystemCardID, "系统日志", "汇总所有运行日志", trimmed
}

func displayWorkerCardTitle(workerKind, workerIndex string) string {
	switch workerKind {
	case "worker":
		return fmt.Sprintf("注册 Worker %s", workerIndex)
	case "auth":
		return fmt.Sprintf("授权 Worker %s", workerIndex)
	default:
		return fmt.Sprintf("Worker %s", workerIndex)
	}
}

func parseLogCardIdentity(cardID string) (kind string, workerIndex string) {
	switch {
	case cardID == tuiSystemCardID:
		return "system", ""
	case strings.HasPrefix(cardID, "worker-"):
		return "worker", strings.TrimPrefix(cardID, "worker-")
	case strings.HasPrefix(cardID, "auth-"):
		return "auth", strings.TrimPrefix(cardID, "auth-")
	default:
		return "other", ""
	}
}

// inferWorkerStatus 基于现有日志文案推断卡片头部状态，不改变任何业务状态机。
// Why: 用户只要求在卡片里补充“最后状态”，直接复用已有日志文本即可避免侵入式修改协议流程。
func inferWorkerStatus(line string, previous string) string {
	switch {
	case strings.Contains(line, "注册成功"):
		return "注册成功"
	case strings.Contains(line, "授权成功"):
		return "授权成功"
	case strings.Contains(line, "注册失败"):
		return "注册失败"
	case strings.Contains(line, "授权失败"):
		return "授权失败"
	case strings.Contains(line, "已收到验证码"):
		return "等待提交通知验证码"
	case strings.Contains(line, "OTP 阶段返回"):
		return "验证码已提交"
	case strings.Contains(line, "准备提交 create_account"):
		return "准备提交资料"
	case strings.Contains(line, "create_account 阶段返回"):
		return "资料已提交"
	case strings.Contains(line, "密码阶段返回"):
		return "密码已提交"
	case strings.Contains(line, "邮箱阶段返回"):
		return "邮箱已提交"
	case strings.Contains(line, "已初始化注册页"):
		return "注册页已初始化"
	case strings.Contains(line, "已初始化登录页"):
		return "登录页已初始化"
	case strings.Contains(line, "开始授权"):
		return "开始授权"
	case strings.Contains(line, "已租到邮箱"):
		return "已租到邮箱"
	case strings.Contains(line, "初始化失败"):
		return "初始化失败"
	default:
		if previous != "" {
			return previous
		}
		return "进行中"
	}
}

func newTUIIntegerInput(value int) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 8
	input.Width = 10
	input.SetValue(fmt.Sprintf("%d", value))
	return input
}

func newTUIStringInput(value string, isPassword bool) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Width = 48
	input.SetValue(value)
	if isPassword {
		input.EchoMode = textinput.EchoPassword
	}
	return input
}

func tuiModeIndex(mode runMode) int {
	for index, option := range tuiModeOptions {
		if option == mode {
			return index
		}
	}
	return 0
}

// displayRunMode 仅负责把内部英文枚举映射成中文展示文案。
// Why: 持久化和业务分发仍依赖稳定的英文值，展示层单独映射才能保证界面中文化且不影响现有逻辑。
func displayRunMode(mode runMode) string {
	switch mode {
	case modeRegister:
		return "仅注册"
	case modeAuthorize:
		return "仅授权"
	case modePipeline:
		return "注册+授权"
	case modeLogin:
		return "登录调试"
	case modeWebMail:
		return "邮箱服务"
	default:
		return string(mode)
	}
}

// workerCardLayoutForMode 返回首页应预留的注册/授权 worker 卡片数量。
// Why: 不同模式消费的是不同 worker 池，单独抽出布局规则后，首页占位和真正运行时都能复用同一套定义。
func workerCardLayoutForMode(mode runMode, workers int, authorizeWorkers int) (int, int) {
	switch mode {
	case modeRegister:
		return maxInt(workers, 1), 0
	case modeAuthorize:
		return 0, maxInt(workers, 1)
	case modePipeline:
		return maxInt(workers, 1), 0
	default:
		return 0, 0
	}
}

// parsePreviewPositiveInt 以“宽松预览”的方式解析配置页数字字段。
// Why: 首页占位只需要尽量贴近用户输入，没必要像真正启动那样因为临时半成品输入而直接报错。
func parsePreviewPositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return maxInt(fallback, 1)
	}
	return value
}

// displayModeConfigHint 返回当前模式下最关键的配置语义提示。
// Why: TUI 在同一页展示了多种模式的参数，必须把“哪些字段会被消费”直接写清，避免用户误以为所有字段都会参与当前流程。
func displayModeConfigHint(mode runMode) string {
	switch mode {
	case modeRegister:
		return "当前模式从邮箱池租用账号注册；“登录邮箱/登录密码/账号文件”不会参与注册流程。"
	case modeAuthorize:
		return "当前模式从 accounts.txt 读取待授权账号；“登录邮箱/登录密码/账号文件”不会参与授权流程。"
	case modePipeline:
		return "当前模式会在同一 worker 内串行执行注册+授权；“授权并发”仅为兼容旧配置保留，不再拆分账号内授权线程。"
	case modeLogin:
		return "当前模式只调试单账号登录；优先使用“登录邮箱/登录密码”，未填写时才回退到“账号文件”。"
	default:
		return "请先选择运行模式。"
	}
}

// loginCredentialFieldHint 返回登录相关字段在当前模式下的生效说明。
// Why: 同样的输入框在不同 mode 下意义完全不同，逐字段补充提示能直接回答“为什么这个字段看起来没作用”。
func loginCredentialFieldHint(mode runMode, fieldName string) string {
	switch mode {
	case modeLogin:
		switch fieldName {
		case "email":
			return "仅登录调试使用；需与登录密码同时填写"
		case "password":
			return "仅登录调试使用；已填写邮箱时必须同时提供"
		case "user-file":
			return "仅登录调试使用；未填写邮箱/密码时回退读取"
		default:
			return ""
		}
	default:
		switch fieldName {
		case "email", "password", "user-file":
			return "当前模式忽略"
		default:
			return ""
		}
	}
}

// renderConfigValueWithHint 在字段值后拼接弱化提示，避免配置表单看起来像所有输入都同等生效。
// Why: TUI 是单屏密集表单，提示必须紧跟在字段后面，用户切换模式时才能立刻看到哪些值会被忽略。
func renderConfigValueWithHint(value string, hint string) string {
	if strings.TrimSpace(hint) == "" {
		return value
	}
	return value + " " + tuiMutedStyle.Render(hint)
}

// buildTUIRunConfig 把单页表单中的字符串值转换成最终运行配置。
// Why: 解析和校验逻辑单独收口后，TUI 层只管交互表现，测试也能直接覆盖核心规则。
func buildTUIRunConfig(
	base config,
	mode runMode,
	webMailURLValue string,
	emailValue string,
	passwordValue string,
	userFileValue string,
	authDirValue string,
	accountsFileValue string,
	cpaURLValue string,
	cpaKeyValue string,
	proxyValue string,
	mailboxValue string,
	countValue string,
	workersValue string,
	authorizeWorkersValue string,
	timeoutValue string,
	otpTimeoutValue string,
	pollIntervalValue string,
	requestTimeoutValue string,
) (config, error) {
	cfg := base
	cfg.mode = mode
	cfg.webMailURL = strings.TrimSpace(webMailURLValue)
	cfg.email = strings.TrimSpace(emailValue)
	cfg.password = strings.TrimSpace(passwordValue)
	cfg.userFile = strings.TrimSpace(userFileValue)
	cfg.authDir = strings.TrimSpace(authDirValue)
	cfg.accountsFile = strings.TrimSpace(accountsFileValue)
	cfg.cpaURL = strings.TrimSpace(cpaURLValue)
	cfg.cpaKey = strings.TrimSpace(cpaKeyValue)
	cfg.proxy = strings.TrimSpace(proxyValue)
	cfg.mailbox = strings.TrimSpace(mailboxValue)

	count, err := parsePositiveTUIInt("count", countValue)
	if err != nil {
		return config{}, err
	}
	workers, err := parsePositiveTUIInt("workers", workersValue)
	if err != nil {
		return config{}, err
	}
	authorizeWorkers, err := parsePositiveTUIInt("authorize-workers", authorizeWorkersValue)
	if err != nil {
		return config{}, err
	}

	cfg.count = count
	cfg.workers = workers
	cfg.authorizeWorkers = authorizeWorkers

	overallTimeout, err := parseDurationValue("timeout", timeoutValue)
	if err != nil {
		return config{}, err
	}
	otpTimeout, err := parseDurationValue("otp-timeout", otpTimeoutValue)
	if err != nil {
		return config{}, err
	}
	pollInterval, err := parseDurationValue("poll-interval", pollIntervalValue)
	if err != nil {
		return config{}, err
	}
	requestTimeout, err := parseDurationValue("request-timeout", requestTimeoutValue)
	if err != nil {
		return config{}, err
	}

	cfg.overallTimeout = overallTimeout
	cfg.otpTimeout = otpTimeout
	cfg.pollInterval = pollInterval
	cfg.requestTimeout = requestTimeout
	return normalizeConfig(cfg)
}

func parsePositiveTUIInt(fieldName, raw string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s必须是大于 0 的整数", displayFieldName(fieldName))
	}
	return value, nil
}

func parseDurationValue(fieldName, raw string) (time.Duration, error) {
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s必须是合法且大于 0 的时长，例如 20s、3m、1h", displayFieldName(fieldName))
	}
	return value, nil
}

func displayFieldName(fieldName string) string {
	switch fieldName {
	case "web-mail-url":
		return "邮箱服务地址"
	case "email":
		return "登录邮箱"
	case "password":
		return "登录密码"
	case "user-file":
		return "账号文件"
	case "auth-dir":
		return "授权目录"
	case "accounts-file":
		return "状态文件"
	case "cpa-url":
		return "CPA 地址"
	case "cpa-key":
		return "CPA 密钥"
	case "proxy":
		return "代理地址"
	case "mailbox":
		return "邮箱目录"
	case "count":
		return "注册数量"
	case "workers":
		return "并发数量"
	case "authorize-workers":
		return "授权并发"
	case "timeout":
		return "整体超时"
	case "otp-timeout":
		return "验证码超时"
	case "poll-interval":
		return "轮询间隔"
	case "request-timeout":
		return "请求超时"
	default:
		return fieldName
	}
}

func loadPersistedTUIConfig(base config) (config, error) {
	return loadPersistedTUIConfigFromPath(filepath.Join(".", tuiConfigFile), base)
}

// loadPersistedTUIConfigFromPath 从本地 JSON 恢复 TUI 页面的最后一次配置。
// Why: TUI 配置需要跨进程复用，但又不应影响其它 CLI 参数来源，因此只在进入 TUI 前做一次定向覆盖。
func loadPersistedTUIConfigFromPath(path string, base config) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return base, nil
		}
		return base, err
	}

	var persisted tuiPersistentConfig
	if err := json.Unmarshal(raw, &persisted); err != nil {
		return base, err
	}

	cfg := base
	if persisted.Mode != "" {
		cfg.mode = runMode(strings.TrimSpace(persisted.Mode))
	}
	if persisted.WebMailURL != "" {
		cfg.webMailURL = strings.TrimSpace(persisted.WebMailURL)
	}
	cfg.email = persisted.Email
	cfg.password = persisted.Password
	if persisted.UserFile != "" {
		cfg.userFile = strings.TrimSpace(persisted.UserFile)
	}
	if persisted.AuthDir != "" {
		cfg.authDir = strings.TrimSpace(persisted.AuthDir)
	}
	if persisted.AccountsFile != "" {
		cfg.accountsFile = strings.TrimSpace(persisted.AccountsFile)
	}
	cfg.cpaURL = strings.TrimSpace(persisted.CPAURL)
	cfg.cpaKey = strings.TrimSpace(persisted.CPAKey)
	cfg.proxy = strings.TrimSpace(persisted.Proxy)
	if persisted.Mailbox != "" {
		cfg.mailbox = strings.TrimSpace(persisted.Mailbox)
	}
	if persisted.Count > 0 {
		cfg.count = persisted.Count
	}
	if persisted.Workers > 0 {
		cfg.workers = persisted.Workers
	}
	if persisted.AuthorizeWorkers > 0 {
		cfg.authorizeWorkers = persisted.AuthorizeWorkers
	}
	if persisted.Timeout != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(persisted.Timeout))
		if err != nil {
			return base, fmt.Errorf("timeout 不合法: %w", err)
		}
		cfg.overallTimeout = duration
	}
	if persisted.OTPTimeout != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(persisted.OTPTimeout))
		if err != nil {
			return base, fmt.Errorf("otp-timeout 不合法: %w", err)
		}
		cfg.otpTimeout = duration
	}
	if persisted.PollInterval != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(persisted.PollInterval))
		if err != nil {
			return base, fmt.Errorf("poll-interval 不合法: %w", err)
		}
		cfg.pollInterval = duration
	}
	if persisted.RequestTimeout != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(persisted.RequestTimeout))
		if err != nil {
			return base, fmt.Errorf("request-timeout 不合法: %w", err)
		}
		cfg.requestTimeout = duration
	}
	return normalizeConfig(cfg)
}

func savePersistedTUIConfig(cfg config) error {
	return savePersistedTUIConfigToPath(filepath.Join(".", tuiConfigFile), cfg)
}

// savePersistedTUIConfigToPath 使用临时文件 + rename 持久化配置，避免中断时留下半写入 JSON。
// Why: TUI 配置文件会在每次启动前被读取，写入必须尽量原子，才能避免用户下次打开时因损坏文件被阻塞。
func savePersistedTUIConfigToPath(path string, cfg config) error {
	payload, err := json.MarshalIndent(tuiPersistentConfig{
		Mode:             string(cfg.mode),
		WebMailURL:       cfg.webMailURL,
		Email:            cfg.email,
		Password:         cfg.password,
		UserFile:         cfg.userFile,
		AuthDir:          cfg.authDir,
		AccountsFile:     cfg.accountsFile,
		CPAURL:           cfg.cpaURL,
		CPAKey:           cfg.cpaKey,
		Proxy:            cfg.proxy,
		Mailbox:          cfg.mailbox,
		Count:            cfg.count,
		Workers:          cfg.workers,
		AuthorizeWorkers: cfg.authorizeWorkers,
		Timeout:          cfg.overallTimeout.String(),
		OTPTimeout:       cfg.otpTimeout.String(),
		PollInterval:     cfg.pollInterval.String(),
		RequestTimeout:   cfg.requestTimeout.String(),
	}, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// formatAverageRegisterSpeed 用“首个注册开始时间 -> 最近一次注册结束时间”的窗口计算平均吞吐。
// Why: 速度指标要反映真实批处理吞吐，而不是把任务结束后的清理时间也算进去，避免结果被无意义拉慢。
func formatAverageRegisterSpeed(startedAt, finishedAt time.Time, completed int) string {
	if completed <= 0 || startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return "--"
	}

	average := finishedAt.Sub(startedAt) / time.Duration(completed)
	if average < 10*time.Second {
		return fmt.Sprintf("%.1f秒/个", average.Seconds())
	}
	return fmt.Sprintf("%.0f秒/个", average.Seconds())
}
