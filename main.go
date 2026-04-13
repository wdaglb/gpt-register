package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type runMode string

const (
	modeRegister  runMode = "register"
	modeAuthorize runMode = "authorize"
	modePipeline  runMode = "pipeline"
	modeLogin     runMode = "login"
	modeWebMail   runMode = "webmail"
)

// config 汇总命令行参数和运行期超时配置。
// Why: 注册和登录两种模式共享同一套网络、邮箱池与超时参数，集中收口后更容易保证两条链路行为一致。
type config struct {
	mode                       runMode
	webMailURL                 string
	webMailHost                string
	webMailPort                int
	webMailDBPath              string
	webMailEmailsFile          string
	mailAPIBase                string
	webMailSyncOnly            bool
	webMailLeaseTimeoutSeconds int
	email                      string
	password                   string
	userFile                   string
	authDir                    string
	accountsFile               string
	proxy                      string
	mailbox                    string
	count                      int
	workers                    int
	authorizeWorkers           int
	overallTimeout             time.Duration
	otpTimeout                 time.Duration
	pollInterval               time.Duration
	requestTimeout             time.Duration
}

type loginAccount struct {
	email    string
	password string
}

func main() {
	usedTUI, err := run(context.Background(), os.Args[1:])
	if err != nil {
		// Why: TUI 模式下错误已经写入滚动日志区，主进程这里不再额外向终端直出，避免退出时再刷一遍原始日志。
		if !usedTUI {
			log.Printf("执行失败: %v", err)
		}
		os.Exit(1)
	}
}

// run 负责把命令行解析、日志初始化和模式分发三件事串起来。
// Why: 入口保持很薄，后续无论新增 register/login 之外的模式，变更点都能稳定收敛在这里。
func run(parent context.Context, args []string) (bool, error) {
	cfg, err := parseConfig(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return false, err
	}

	if cfg.mode == modeWebMail {
		ui := newPlainProgressUI(os.Stdout)
		logger := log.New(ui.LogWriter(), "[go-register] ", log.LstdFlags|log.Lmsgprefix)
		return false, runWebMailServer(parent, cfg, logger)
	}

	mailClient := newWebMailClient(cfg.webMailURL, cfg.requestTimeout)
	store := newAccountsStore(cfg.accountsFile)

	if shouldUseTUI(cfg) {
		return true, runWithTUI(parent, cfg, mailClient, store)
	}

	ui := newPlainProgressUI(os.Stdout)
	logger := log.New(ui.LogWriter(), "[go-register] ", log.LstdFlags|log.Lmsgprefix)
	return false, executeMode(parent, cfg, mailClient, logger, store, ui)
}

// executeMode 统一分发各运行模式，避免普通日志模式和 TUI 模式复制一份业务入口。
// Why: UI 渲染只是表现层差异，真正的注册/授权逻辑应共用同一条调度链路，避免后续改业务时漏改某个入口。
func executeMode(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI) error {
	if store != nil {
		store.setWaitLogger(logger)
	}
	if mailClient != nil && logger != nil {
		mailClient.setWaitLogger(logger.Printf)
	}
	switch cfg.mode {
	case modeRegister:
		return runRegister(parent, cfg, mailClient, logger, store, ui, false)
	case modeAuthorize:
		return runAuthorizeFromAccounts(parent, cfg, mailClient, logger, store, ui)
	case modePipeline:
		return runPipeline(parent, cfg, mailClient, logger, store, ui)
	case modeLogin:
		return runLogin(parent, cfg, mailClient, logger, store, ui)
	default:
		return fmt.Errorf("不支持的 mode: %s", cfg.mode)
	}
}

// runLogin 只负责单账号登录闭环，并把最终结果落到结果文件。
// Why: 登录链路依赖本地 auth 文件生成，和注册的批量 worker 语义不同，因此显式拆开实现，避免两种流程互相污染。
func runLogin(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI) error {
	account, err := resolveAccount(cfg)
	if err != nil {
		return err
	}

	logger.Printf("准备登录账号: %s", account.email)
	threadLabel := "login"
	resolvedCfg, flowIP := prepareFlowLogging(parent, cfg, logger, threadLabel)
	loginPrefix := buildFlowLogPrefix(threadLabel, flowIP, account.email)
	flowLogger := newFlowScopedLogger(logger, loginPrefix)
	loginCtx, cancel := context.WithTimeout(parent, cfg.overallTimeout)
	defer cancel()

	logger.Printf("%s 开始登录流程", loginPrefix)
	result, err := loginWithProtocol(loginCtx, resolvedCfg, account, mailClient, flowLogger)
	if err != nil {
		ui.RecordAuthorizeFinish(false)
		if store != nil {
			reason := summarizeFlowReason(err)
			if _, writeErr := store.upsertOAuthResult(account.email, account.password, "oauth=fail:"+reason, time.Now(), ""); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		logger.Printf("%s 登录失败: %v", loginPrefix, err)
		return err
	}

	ui.RecordAuthorizeFinish(true)
	if store != nil {
		if _, err := store.upsertOAuthResult(account.email, account.password, "oauth=ok", time.Now(), result.AuthFilePath); err != nil {
			return err
		}
	}
	logger.Printf("%s 授权文件已生成: %s", loginPrefix, result.AuthFilePath)
	logger.Printf("%s callback=%s", loginPrefix, result.CallbackURL)
	return nil
}

// parseConfig 在统一位置处理参数合法性和默认值兜底。
// Why: 这一步先把“不完整配置”挡在业务逻辑之外，后面的协议代码就可以假设字段都已经可用。
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("go-register", flag.ContinueOnError)

	cfg := config{}
	fs.StringVar((*string)(&cfg.mode), "mode", envOrDefault("GO_REGISTER_MODE", string(modeRegister)), "执行模式: register/authorize/pipeline/login/webmail")
	fs.StringVar(&cfg.webMailURL, "web-mail-url", envOrDefault("WEB_MAIL_URL", "http://127.0.0.1:8030"), "web_mail 服务地址")
	fs.StringVar(&cfg.webMailHost, "web-mail-host", envOrDefault("WEB_MAIL_HOST", "127.0.0.1"), "web_mail 服务监听地址，仅 webmail 模式生效")
	fs.IntVar(&cfg.webMailPort, "web-mail-port", 8030, "web_mail 服务监听端口，仅 webmail 模式生效")
	fs.StringVar(&cfg.webMailDBPath, "web-mail-db", envOrDefault("WEB_MAIL_DB", ""), "已废弃参数：历史 SQLite 路径，当前 web_mail 直接使用 web-mail-emails-file 持久化")
	fs.StringVar(&cfg.webMailEmailsFile, "web-mail-emails-file", envOrDefault("WEB_MAIL_EMAILS_FILE", defaultProjectFile("emails.txt")), "邮箱池 txt 数据库文件路径，仅 webmail 模式生效")
	fs.StringVar(&cfg.mailAPIBase, "mail-api-base", envOrDefault("MAIL_API_BASE", defaultMailAPIBaseURL), "上游邮件接口基础地址，仅 webmail 模式生效")
	fs.BoolVar(&cfg.webMailSyncOnly, "web-mail-sync-only", false, "仅执行一次邮箱同步后退出，仅 webmail 模式生效")
	fs.IntVar(&cfg.webMailLeaseTimeoutSeconds, "web-mail-lease-timeout-seconds", defaultWebMailLeaseTimeoutSeconds, "邮箱租约超时秒数，仅 webmail 模式生效")
	fs.StringVar(&cfg.email, "email", "", "OpenAI 账号邮箱；为空时从 user-file 读取")
	fs.StringVar(&cfg.password, "password", "", "OpenAI 账号密码；为空时从 user-file 读取")
	fs.StringVar(&cfg.userFile, "user-file", defaultUserFile(), "账号文件路径，支持两行 email/password 或单行 email----password")
	fs.StringVar(&cfg.authDir, "auth-dir", "auth", "授权文件输出目录")
	fs.StringVar(&cfg.accountsFile, "accounts-file", "", "账号状态文件路径，默认 accounts.txt")
	fs.StringVar(&cfg.proxy, "proxy", "http://127.0.0.1:7890", "HTTP/HTTPS 代理地址，例如 http://127.0.0.1:7890")
	fs.StringVar(&cfg.mailbox, "mailbox", "Junk", "验证码轮询优先邮箱目录，默认先查 Junk 再回退 INBOX")
	fs.IntVar(&cfg.count, "count", 1, "register/pipeline 模式下的注册数量")
	fs.IntVar(&cfg.workers, "workers", 1, "register/authorize 模式下的并发数")
	fs.IntVar(&cfg.authorizeWorkers, "authorize-workers", 1, "pipeline 模式下的授权并发数")
	fs.DurationVar(&cfg.overallTimeout, "timeout", 4*time.Minute, "整条登录流程超时")
	fs.DurationVar(&cfg.otpTimeout, "otp-timeout", 90*time.Second, "单次验证码等待超时")
	fs.DurationVar(&cfg.pollInterval, "poll-interval", 3*time.Second, "验证码轮询间隔")
	fs.DurationVar(&cfg.requestTimeout, "request-timeout", 20*time.Second, "单次 HTTP 请求超时")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	return normalizeConfig(cfg)
}

// normalizeConfig 统一执行配置归一化与合法性校验。
// Why: 现在 CLI 参数和 TUI 页面都能生成运行配置，公共规则必须收敛到一个函数，避免两边出现不一致。
func normalizeConfig(cfg config) (config, error) {
	cfg.mode = runMode(strings.ToLower(strings.TrimSpace(string(cfg.mode))))
	if cfg.mode != modeRegister && cfg.mode != modeAuthorize && cfg.mode != modePipeline && cfg.mode != modeLogin && cfg.mode != modeWebMail {
		return config{}, fmt.Errorf("mode 仅支持 register/authorize/pipeline/login/webmail")
	}
	if cfg.webMailURL == "" {
		return config{}, fmt.Errorf("web_mail 地址不能为空")
	}
	if cfg.webMailHost == "" {
		cfg.webMailHost = "127.0.0.1"
	}
	if cfg.webMailPort <= 0 {
		cfg.webMailPort = 8030
	}
	if cfg.webMailPort > 65535 {
		return config{}, fmt.Errorf("web-mail-port 必须在 1~65535 之间")
	}
	if cfg.webMailEmailsFile == "" {
		cfg.webMailEmailsFile = defaultProjectFile("emails.txt")
	}
	if cfg.mailAPIBase == "" {
		cfg.mailAPIBase = defaultMailAPIBaseURL
	}
	if cfg.webMailLeaseTimeoutSeconds <= 0 {
		cfg.webMailLeaseTimeoutSeconds = defaultWebMailLeaseTimeoutSeconds
	}
	if cfg.authDir == "" {
		return config{}, fmt.Errorf("auth-dir 不能为空")
	}
	if cfg.mailbox == "" {
		cfg.mailbox = "Junk"
	}
	if cfg.accountsFile == "" {
		cfg.accountsFile = "accounts.txt"
	}
	if cfg.count <= 0 {
		return config{}, fmt.Errorf("count 必须大于 0")
	}
	if cfg.workers <= 0 {
		return config{}, fmt.Errorf("workers 必须大于 0")
	}
	if cfg.authorizeWorkers <= 0 {
		return config{}, fmt.Errorf("authorize-workers 必须大于 0")
	}
	if cfg.pollInterval <= 0 {
		return config{}, fmt.Errorf("poll-interval 必须大于 0")
	}
	if cfg.requestTimeout <= 0 {
		return config{}, fmt.Errorf("request-timeout 必须大于 0")
	}
	if cfg.otpTimeout <= 0 {
		return config{}, fmt.Errorf("otp-timeout 必须大于 0")
	}
	if cfg.overallTimeout <= 0 {
		return config{}, fmt.Errorf("timeout 必须大于 0")
	}

	cfg.authDir = filepath.Clean(cfg.authDir)
	if cfg.webMailDBPath != "" {
		cfg.webMailDBPath = filepath.Clean(cfg.webMailDBPath)
	}
	cfg.webMailEmailsFile = filepath.Clean(cfg.webMailEmailsFile)
	return cfg, nil
}

func resolveAccount(cfg config) (loginAccount, error) {
	// Why: 显式传参优先，便于临时调试单个账号；未传参时再回退到 user.txt。
	if cfg.email != "" || cfg.password != "" {
		if cfg.email == "" || cfg.password == "" {
			return loginAccount{}, fmt.Errorf("email/password 需要同时提供")
		}
		return loginAccount{email: cfg.email, password: cfg.password}, nil
	}

	if cfg.userFile == "" {
		return loginAccount{}, fmt.Errorf("未提供 email/password，且 user-file 为空")
	}

	raw, err := os.ReadFile(cfg.userFile)
	if err != nil {
		return loginAccount{}, fmt.Errorf("读取 user-file 失败: %w", err)
	}

	lines := make([]string, 0, 2)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) == 1 && strings.Contains(lines[0], "----") {
		parts := strings.Split(lines[0], "----")
		if len(parts) >= 2 {
			return loginAccount{
				email:    strings.TrimSpace(parts[0]),
				password: strings.TrimSpace(parts[1]),
			}, nil
		}
	}

	if len(lines) < 2 {
		return loginAccount{}, fmt.Errorf("user-file 格式不正确，至少需要邮箱和密码两行")
	}

	return loginAccount{
		email:    lines[0],
		password: lines[1],
	}, nil
}

// defaultUserFile 兼容历史单账号调试习惯。
// Why: 只有在当前目录已有 user.txt 时才自动兜底，避免误把空字符串当成有效配置。
func defaultUserFile() string {
	if _, err := os.Stat("user.txt"); err == nil {
		return "user.txt"
	}
	return ""
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

// defaultProjectFile 返回当前工作目录下的默认项目文件绝对路径。
// Why: webmail 服务既支持显式传参，也需要在仓库内自洽运行，因此默认值直接指向当前项目根目录更直观。
func defaultProjectFile(name string) string {
	workDir, err := os.Getwd()
	if err != nil {
		return name
	}
	return filepath.Join(workDir, name)
}

func appendAccountResult(path string, account loginAccount, status, reason string) error {
	line := fmt.Sprintf("%s----%s----%s----%s\n", account.email, account.password, status, reason)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("写入 accounts 结果失败: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	if _, err := file.WriteString(line); err != nil {
		return fmt.Errorf("写入 accounts 结果失败: %w", err)
	}
	return nil
}
