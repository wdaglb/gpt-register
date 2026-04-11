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
)

// config 汇总命令行参数和运行期超时配置。
// Why: 注册和登录两种模式共享同一套网络、邮箱池与超时参数，集中收口后更容易保证两条链路行为一致。
type config struct {
	mode             runMode
	webMailURL       string
	email            string
	password         string
	userFile         string
	authDir          string
	accountsFile     string
	proxy            string
	mailbox          string
	count            int
	workers          int
	authorizeWorkers int
	overallTimeout   time.Duration
	otpTimeout       time.Duration
	pollInterval     time.Duration
	requestTimeout   time.Duration
}

type loginAccount struct {
	email    string
	password string
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Printf("执行失败: %v", err)
		os.Exit(1)
	}
}

// run 负责把命令行解析、日志初始化和模式分发三件事串起来。
// Why: 入口保持很薄，后续无论新增 register/login 之外的模式，变更点都能稳定收敛在这里。
func run(parent context.Context, args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	logger := log.New(os.Stdout, "[go-register] ", log.LstdFlags|log.Lmsgprefix)
	mailClient := newWebMailClient(cfg.webMailURL, cfg.requestTimeout)
	store := newAccountsStore(cfg.accountsFile)

	switch cfg.mode {
	case modeRegister:
		return runRegister(parent, cfg, mailClient, logger, store, nil)
	case modeAuthorize:
		return runAuthorizeFromAccounts(parent, cfg, mailClient, logger, store)
	case modePipeline:
		return runPipeline(parent, cfg, mailClient, logger, store)
	case modeLogin:
		return runLogin(parent, cfg, mailClient, logger, store)
	default:
		return fmt.Errorf("不支持的 mode: %s", cfg.mode)
	}
}

// runLogin 只负责单账号登录闭环，并把最终结果落到结果文件。
// Why: 登录链路依赖本地 auth 文件生成，和注册的批量 worker 语义不同，因此显式拆开实现，避免两种流程互相污染。
func runLogin(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore) error {
	account, err := resolveAccount(cfg)
	if err != nil {
		return err
	}

	logger.Printf("准备登录账号: %s", account.email)
	loginCtx, cancel := context.WithTimeout(parent, cfg.overallTimeout)
	defer cancel()

	result, err := loginWithProtocol(loginCtx, cfg, account, mailClient, logger)
	if err != nil {
		if store != nil {
			reason := summarizeFlowReason(err)
			if _, writeErr := store.upsertOAuthResult(account.email, account.password, "oauth=fail:"+reason, time.Now(), ""); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		return err
	}

	if store != nil {
		if _, err := store.upsertOAuthResult(account.email, account.password, "oauth=ok", time.Now(), result.AuthFilePath); err != nil {
			return err
		}
	}
	logger.Printf("授权文件已生成: %s", result.AuthFilePath)
	logger.Printf("callback=%s", result.CallbackURL)
	return nil
}

// parseConfig 在统一位置处理参数合法性和默认值兜底。
// Why: 这一步先把“不完整配置”挡在业务逻辑之外，后面的协议代码就可以假设字段都已经可用。
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("go-register", flag.ContinueOnError)

	cfg := config{}
	fs.StringVar((*string)(&cfg.mode), "mode", envOrDefault("GO_REGISTER_MODE", string(modeRegister)), "执行模式: register/authorize/pipeline/login")
	fs.StringVar(&cfg.webMailURL, "web-mail-url", envOrDefault("WEB_MAIL_URL", "http://127.0.0.1:8030"), "web_mail 服务地址")
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

	cfg.mode = runMode(strings.ToLower(strings.TrimSpace(string(cfg.mode))))
	if cfg.mode != modeRegister && cfg.mode != modeAuthorize && cfg.mode != modePipeline && cfg.mode != modeLogin {
		return config{}, fmt.Errorf("mode 仅支持 register/authorize/pipeline/login")
	}
	if cfg.webMailURL == "" {
		return config{}, fmt.Errorf("web_mail 地址不能为空")
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
