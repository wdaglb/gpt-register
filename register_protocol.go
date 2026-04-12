package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

const (
	registerCSRFURL            = "https://chatgpt.com/api/auth/csrf"
	registerSignInURL          = "https://chatgpt.com/api/auth/signin/openai"
	registerAuthorizeURL       = "https://auth.openai.com/api/accounts/authorize/continue"
	registerPasswordURL        = "https://auth.openai.com/api/accounts/user/register"
	registerSendEmailOTPURL    = "https://auth.openai.com/api/accounts/email-otp/send"
	registerCreateAccountURL   = "https://auth.openai.com/api/accounts/create_account"
	registerStartPageURL       = "https://auth.openai.com/create-account"
	registerPasswordPageURL    = "https://auth.openai.com/create-account/password"
	registerVerifyEmailPageURL = "https://auth.openai.com/create-account/verify-email"
	registerAboutYouPageURL    = "https://auth.openai.com/about-you"
)

// registrationAttemptResult 记录单个注册任务的持久化结果。
// Why: worker 并发执行时，只把摘要结果传回聚合层，避免直接在 goroutine 里竞争写文件。
type registrationAttemptResult struct {
	Email    string
	Password string
	Status   string
	Reason   string
	Err      error
}

type registerSignInResponse struct {
	URL string `json:"url"`
}

type csrfResponse struct {
	CSRFToken string `json:"csrfToken"`
}

type createAccountPayload struct {
	Name      string `json:"name"`
	Birthdate string `json:"birthdate"`
}

// runRegister 负责批量注册的调度、结果聚合和落盘。
// Why: 这里把并发控制、TUI 统计更新和协议细节解耦，后续即使要增加重试或限速，也不需要改动底层注册请求顺序。
func runRegister(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, authorizeJobs chan<- accountRecord) error {
	if cfg.count <= 0 {
		return fmt.Errorf("register 模式下 count 必须大于 0")
	}
	if cfg.workers <= 0 {
		return fmt.Errorf("register 模式下 workers 必须大于 0")
	}

	workerCount := cfg.workers
	if workerCount > cfg.count {
		workerCount = cfg.count
	}

	jobs := make(chan int)
	results := make(chan registrationAttemptResult, workerCount)

	var workers sync.WaitGroup
	for workerID := 1; workerID <= workerCount; workerID++ {
		workerID := workerID
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range jobs {
				ui.RecordRegisterStart()
				result := runRegisterAttempt(parent, cfg, mailClient, logger, store, authorizeJobs, workerID)
				ui.RecordRegisterFinish(result.Status == "ok")
				results <- result
			}
		}()
	}

	go func() {
		for i := 0; i < cfg.count; i++ {
			jobs <- i + 1
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	success := 0
	fail := 0
	var firstErr error
	for result := range results {
		switch result.Status {
		case "ok":
			success++
		default:
			fail++
			logger.Printf("注册失败账号=%s reason=%s err=%v", result.Email, result.Reason, result.Err)
			if firstErr == nil && result.Err != nil {
				firstErr = result.Err
			}
		}
	}

	logger.Printf("注册任务结束: success=%d fail=%d output=%s", success, fail, cfg.accountsFile)
	if success == 0 && fail > 0 {
		if firstErr != nil {
			return fmt.Errorf("全部注册失败: %w", firstErr)
		}
		return fmt.Errorf("全部注册失败")
	}
	return nil
}

// runRegisterAttempt 负责单个邮箱租约的一次完整注册尝试。
// 核心流程：租号 -> 纯协议注册 -> 立即写入 accounts.txt -> 成功标记 used / 失败归还租约。
func runRegisterAttempt(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, authorizeJobs chan<- accountRecord, workerID int) registrationAttemptResult {
	attemptCtx, cancel := context.WithTimeout(parent, cfg.overallTimeout)
	defer cancel()

	result := registrationAttemptResult{
		Status: "fail",
		Reason: "lease_account",
	}

	lease, err := mailClient.leaseAccount(attemptCtx)
	if err != nil {
		result.Err = fmt.Errorf("租用邮箱失败: %w", err)
		result.Reason = "lease_account"
		logger.Printf("[worker-%d] 租用邮箱失败: %v", workerID, err)
		return result
	}

	result.Email = lease.Email
	result.Password = generateRegistrationPassword()
	prefix := fmt.Sprintf("[worker-%d][%s]", workerID, lease.Email)
	logger.Printf("%s 已租到邮箱 account_id=%d", prefix, lease.ID)

	client, err := newProtocolClient(cfg)
	if err != nil {
		result.Err = fmt.Errorf("创建协议客户端失败: %w", err)
		result.Reason = "client_init"
		if store != nil {
			if _, writeErr := store.upsertRegistration(result.Email, result.Password, "fail:"+result.Reason, time.Now()); writeErr != nil {
				result.Err = fmt.Errorf("%v；写入 accounts 失败: %w", result.Err, writeErr)
			}
		}
		returnAccountWithCleanup(cfg, mailClient, lease, logger, prefix)
		logger.Printf("%s 初始化失败: %v", prefix, err)
		return result
	}

	if err := client.registerWithProtocol(attemptCtx, cfg, lease, result.Password, mailClient, logger, prefix); err != nil {
		result.Err = err
		result.Reason = summarizeFlowReason(err)
		if store != nil {
			if _, writeErr := store.upsertRegistration(result.Email, result.Password, "fail:"+result.Reason, time.Now()); writeErr != nil {
				result.Err = fmt.Errorf("%v；写入 accounts 失败: %w", result.Err, writeErr)
			}
		}
		if shouldKeepRegisterLeaseOnInvalidAuthStep(err) {
			// Why: invalid_auth_step 出现在注册密码阶段时，说明该邮箱的注册会话已经走到不可复用状态，
			// 继续归还到池里只会让后续 worker 反复租到同一个坏邮箱，因此这里直接标记为已使用并移出池子。
			markUsedWithCleanup(cfg, mailClient, lease, logger, prefix)
			logger.Printf("%s 注册密码阶段命中 invalid_auth_step，邮箱已移出池子", prefix)
		} else {
			returnAccountWithCleanup(cfg, mailClient, lease, logger, prefix)
		}
		logger.Printf("%s 注册失败: %v", prefix, err)
		return result
	}

	result.Status = "ok"
	result.Reason = "ok"
	result.Err = nil
	if store != nil {
		record, writeErr := store.upsertRegistration(result.Email, result.Password, "ok", time.Now())
		if writeErr != nil {
			result.Status = "fail"
			result.Reason = "accounts_write"
			result.Err = fmt.Errorf("注册成功但写入 accounts 失败: %w", writeErr)
			logger.Printf("%s 注册成功但写入 accounts 失败: %v", prefix, writeErr)
			return result
		}
		if authorizeJobs != nil {
			authorizeJobs <- record
		}
	}
	markUsedWithCleanup(cfg, mailClient, lease, logger, prefix)
	logger.Printf("%s 注册成功", prefix)
	return result
}

// registerWithProtocol 按线上注册页的真实顺序推进整条协议链路。
// Why: 注册阶段的 session、Sentinel token 和邮箱 OTP 都有强时序依赖，拆散后会更难定位是哪一步失效。
func (c *protocolClient) registerWithProtocol(ctx context.Context, cfg config, lease *webMailAccount, password string, mailClient *webMailClient, logger *log.Logger, prefix string) error {
	bootstrap, err := c.bootstrapSignupPage(ctx, lease.Email)
	if err != nil {
		return newFlowResultError("bootstrap_signup", err)
	}
	logger.Printf("%s 已初始化注册页: %s", prefix, bootstrap.FinalURL)

	identifierResult, err := c.submitSignupIdentifier(ctx, cfg, bootstrap, lease.Email)
	if err != nil {
		return newFlowResultError("authorize_continue", err)
	}
	identifierStep := parseAuthAPIEnvelope(identifierResult.Body)
	logger.Printf("%s 邮箱阶段返回: status=%d next=%s", prefix, identifierResult.StatusCode, identifierStep.Page.Type)
	if identifierStep.ContinueURL != "" {
		if err := c.openPage(ctx, identifierStep.ContinueURL, bootstrap.FinalURL); err != nil {
			return newFlowResultError("open_password_page", fmt.Errorf("打开密码页失败: %w", err))
		}
	}

	registerResult, err := c.submitRegisterPassword(ctx, cfg, bootstrap, lease.Email, password)
	if err != nil {
		return newFlowResultError("user_register", err)
	}
	logger.Printf("%s 密码阶段返回: status=%d", prefix, registerResult.StatusCode)

	if err := c.sendRegistrationEmailOTP(ctx); err != nil {
		return newFlowResultError("send_otp", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, cfg.otpTimeout)
	defer cancel()

	// Why: web_mail 时间戳精度只有秒，回看 15 秒可以避免刚触发的验证码被误判成旧邮件。
	code, err := mailClient.waitCodeByAccount(waitCtx, lease.ID, cfg.mailbox, cfg.pollInterval, time.Now().UTC().Add(-15*time.Second))
	if err != nil {
		return newFlowResultError("otp_wait", fmt.Errorf("等待邮箱验证码失败: %w", err))
	}
	logger.Printf("%s 已收到验证码，准备提交", prefix)

	validateResult, err := c.validateRegisterEmailOTP(ctx, code)
	if err != nil {
		return newFlowResultError("otp_validate", err)
	}
	logger.Printf("%s OTP 阶段返回: status=%d", prefix, validateResult.StatusCode)

	profile := randomCreateAccountPayload()
	logger.Printf("%s 准备提交 create_account", prefix)
	createAccountResult, err := c.submitCreateAccount(ctx, cfg, bootstrap, profile)
	if err != nil {
		return newFlowResultError("create_account", err)
	}
	logger.Printf("%s create_account 阶段返回: status=%d", prefix, createAccountResult.StatusCode)
	return nil
}

// bootstrapSignupPage 先从 chatgpt.com 取 csrf 和跳转 URL，再落到 auth.openai.com 注册页。
// Why: 直接硬打 auth.openai.com/create-account 会丢失前置站点写入的 cookie 与 did，导致后续会话不完整。
func (c *protocolClient) bootstrapSignupPage(ctx context.Context, email string) (*bootstrapPage, error) {
	if _, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           "https://chatgpt.com",
		AllowRedirect: true,
		Accept:        auth0AcceptHeader,
		Profile:       profileNavigate,
	}); err != nil {
		return nil, fmt.Errorf("初始化 chatgpt.com 失败: %w", err)
	}

	csrfResult, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           registerCSRFURL,
		AllowRedirect: true,
		Accept:        "application/json",
		Referer:       "https://chatgpt.com/",
		Profile:       profileAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("获取 csrf token 失败: %w", err)
	}
	if csrfResult.StatusCode < 200 || csrfResult.StatusCode >= 300 {
		return nil, fmt.Errorf("获取 csrf token 失败: HTTP %d body=%s", csrfResult.StatusCode, limitBody(csrfResult.Body))
	}

	csrfPayload := csrfResponse{}
	if err := json.Unmarshal([]byte(csrfResult.Body), &csrfPayload); err != nil {
		return nil, fmt.Errorf("解析 csrf token 响应失败: %w", err)
	}
	if strings.TrimSpace(csrfPayload.CSRFToken) == "" {
		return nil, fmt.Errorf("csrf token 为空")
	}

	params := url.Values{}
	params.Set("prompt", "login")
	params.Set("ext-oai-did", randomUUID())
	params.Set("screen_hint", "login_or_signup")
	params.Set("login_hint", email)

	signInResult, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           registerSignInURL + "?" + params.Encode(),
		AllowRedirect: true,
		Accept:        "application/json",
		ContentType:   "application/x-www-form-urlencoded",
		Referer:       "https://chatgpt.com/",
		Origin:        "https://chatgpt.com",
		Profile:       profileAPI,
		Form: url.Values{
			"csrfToken":   {csrfPayload.CSRFToken},
			"callbackUrl": {"/"},
			"json":        {"true"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("发起 OpenAI 注册页失败: %w", err)
	}
	if signInResult.StatusCode < 200 || signInResult.StatusCode >= 300 {
		return nil, fmt.Errorf("发起 OpenAI 注册页失败: HTTP %d body=%s", signInResult.StatusCode, limitBody(signInResult.Body))
	}

	signInPayload := registerSignInResponse{}
	if err := json.Unmarshal([]byte(signInResult.Body), &signInPayload); err != nil {
		return nil, fmt.Errorf("解析注册跳转地址失败: %w", err)
	}
	if strings.TrimSpace(signInPayload.URL) == "" {
		return nil, fmt.Errorf("注册跳转地址为空")
	}
	return c.bootstrapLoginPage(ctx, signInPayload.URL)
}

// submitSignupIdentifier 提交注册邮箱，并写入当前 auth session。
func (c *protocolClient) submitSignupIdentifier(ctx context.Context, cfg config, bootstrap *bootstrapPage, email string) (*httpResult, error) {
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, bootstrap, "username_password_create", registerStartPageURL)
	if err != nil {
		return nil, fmt.Errorf("生成 username_password_create sentinel 失败: %w", err)
	}

	payload := strings.NewReader(fmt.Sprintf(`{"username":{"kind":"email","value":%q},"screen_hint":"signup"}`, email))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           registerAuthorizeURL,
		AllowRedirect: true,
		Referer:       registerStartPageURL,
		Origin:        "https://auth.openai.com",
		Accept:        "application/json",
		ContentType:   "application/json",
		Profile:       profileAPI,
		Body:          payload,
		ExtraHeaders:  sentinelHeader,
	})
	if err != nil {
		return nil, fmt.Errorf("提交注册邮箱失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("register_authorize_continue", result.Body)
		return nil, fmt.Errorf("提交注册邮箱返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("register_authorize_continue_success", result)
	return result, nil
}

// submitRegisterPassword 提交注册密码。
// Why: 这里继续沿用 username_password_create flow，是为了与参考脚本和当前前端页面的 Sentinel 语义保持一致。
func (c *protocolClient) submitRegisterPassword(ctx context.Context, cfg config, bootstrap *bootstrapPage, email, password string) (*httpResult, error) {
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, bootstrap, "username_password_create", registerPasswordPageURL)
	if err != nil {
		return nil, fmt.Errorf("生成注册密码 sentinel 失败: %w", err)
	}

	payload := strings.NewReader(fmt.Sprintf(`{"password":%q,"username":%q}`, password, email))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           registerPasswordURL,
		AllowRedirect: true,
		Referer:       registerPasswordPageURL,
		Origin:        "https://auth.openai.com",
		Accept:        "application/json",
		ContentType:   "application/json",
		Profile:       profileAPI,
		Body:          payload,
		ExtraHeaders:  sentinelHeader,
	})
	if err != nil {
		return nil, fmt.Errorf("设置注册密码失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("register_user_register", result.Body)
		return nil, fmt.Errorf("设置注册密码返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("register_user_register_success", result)
	return result, nil
}

// sendRegistrationEmailOTP 触发注册验证码发送。
func (c *protocolClient) sendRegistrationEmailOTP(ctx context.Context) error {
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           registerSendEmailOTPURL,
		AllowRedirect: true,
		Accept:        "application/json",
		Referer:       registerVerifyEmailPageURL,
		Profile:       profileAPI,
	})
	if err != nil {
		return fmt.Errorf("触发邮箱验证码发送失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("register_send_email_otp", result.Body)
		return fmt.Errorf("触发邮箱验证码返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("register_send_email_otp_success", result)
	return nil
}

// validateRegisterEmailOTP 提交邮箱验证码，推进到 about-you 阶段。
func (c *protocolClient) validateRegisterEmailOTP(ctx context.Context, code string) (*httpResult, error) {
	payload := strings.NewReader(fmt.Sprintf(`{"code":%q}`, code))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           "https://auth.openai.com/api/accounts/email-otp/validate",
		AllowRedirect: true,
		Accept:        "application/json",
		ContentType:   "application/json",
		Referer:       registerVerifyEmailPageURL,
		Origin:        "https://auth.openai.com",
		Profile:       profileAPI,
		Body:          payload,
	})
	if err != nil {
		return nil, fmt.Errorf("提交注册验证码失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("register_email_otp_validate", result.Body)
		return nil, fmt.Errorf("提交注册验证码返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("register_email_otp_validate_success", result)
	return result, nil
}

// submitCreateAccount 提交姓名和生日，完成注册链路的最后一步。
func (c *protocolClient) submitCreateAccount(ctx context.Context, cfg config, bootstrap *bootstrapPage, payload createAccountPayload) (*httpResult, error) {
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, bootstrap, "oauth_create_account", registerAboutYouPageURL)
	if err != nil {
		return nil, fmt.Errorf("生成 create_account sentinel 失败: %w", err)
	}

	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化 create_account 请求失败: %w", err)
	}

	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           registerCreateAccountURL,
		AllowRedirect: true,
		Referer:       registerAboutYouPageURL,
		Origin:        "https://auth.openai.com",
		Accept:        "application/json",
		ContentType:   "application/json",
		Profile:       profileAPI,
		Body:          strings.NewReader(string(encodedPayload)),
		ExtraHeaders:  sentinelHeader,
	})
	if err != nil {
		return nil, fmt.Errorf("提交 create_account 失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("register_create_account", result.Body)
		return nil, fmt.Errorf("提交 create_account 返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("register_create_account_success", result)
	return result, nil
}

// appendRegistrationResult 以与旧脚本兼容的格式写入注册结果。
func appendRegistrationResult(path string, result registrationAttemptResult) error {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	status := result.Status
	if status == "" {
		status = "fail"
	}

	line := fmt.Sprintf("%s----%s----%s----%s\n", result.Email, result.Password, status, timestamp)
	if status != "ok" {
		line = fmt.Sprintf("%s----%s----fail:%s----%s\n", result.Email, result.Password, result.Reason, timestamp)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("写入注册结果失败: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	if _, err := file.WriteString(line); err != nil {
		return fmt.Errorf("写入注册结果失败: %w", err)
	}
	return nil
}

// summarizeFlowReason 尽量把复杂错误压缩成稳定的落盘原因字段。
// Why: 结果文件需要适合后续批量筛查，不能直接写入过长的原始 HTTP 响应。
func summarizeFlowReason(err error) string {
	if flowErr, ok := asFlowResultError(err); ok && strings.TrimSpace(flowErr.reason) != "" {
		return flowErr.reason
	}

	message := strings.Join(strings.Fields(err.Error()), " ")
	if message == "" {
		return "unknown"
	}
	if len(message) > 120 {
		return message[:120]
	}
	return message
}

// shouldKeepRegisterLeaseOnInvalidAuthStep 判断注册失败是否需要把邮箱移出池子而不是归还。
// Why: 只有“注册密码阶段 + invalid_auth_step”这一类失败会稳定复现且不可重试，必须避免邮箱被重新租出。
func shouldKeepRegisterLeaseOnInvalidAuthStep(err error) bool {
	flowErr, ok := asFlowResultError(err)
	if !ok || flowErr == nil || strings.TrimSpace(flowErr.reason) != "user_register" {
		return false
	}
	return strings.Contains(flowErr.Error(), "invalid_auth_step")
}

// returnAccountWithCleanup 在主流程失败后归还邮箱租约。
func returnAccountWithCleanup(cfg config, mailClient *webMailClient, lease *webMailAccount, logger *log.Logger, prefix string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout(cfg))
	defer cancel()

	// Why: 主流程上下文可能已经因超时取消，归还租约必须脱离主链路单独兜底，否则邮箱会长期卡在 leased 状态。
	if err := mailClient.returnAccount(cleanupCtx, lease.ID, lease.LeaseToken); err != nil {
		logger.Printf("%s 归还邮箱失败: %v", prefix, err)
	}
}

// markUsedWithCleanup 在注册成功后把邮箱标记为已使用，避免再次被租出。
func markUsedWithCleanup(cfg config, mailClient *webMailClient, lease *webMailAccount, logger *log.Logger, prefix string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout(cfg))
	defer cancel()

	if err := mailClient.markUsed(cleanupCtx, lease.ID, lease.LeaseToken); err != nil {
		logger.Printf("%s 标记邮箱已使用失败: %v", prefix, err)
	}
}

// cleanupTimeout 为租约回写单独提供一个可用超时，避免主流程 ctx 取消后清理动作立即失败。
func cleanupTimeout(cfg config) time.Duration {
	if cfg.requestTimeout > 0 {
		return cfg.requestTimeout
	}
	return 20 * time.Second
}

// generateRegistrationPassword 生成与参考脚本兼容的注册密码格式。
func generateRegistrationPassword() string {
	return randomAlphaNumeric(10) + "aA1!"
}

// randomCreateAccountPayload 构造 about-you 阶段需要的人设信息。
func randomCreateAccountPayload() createAccountPayload {
	return createAccountPayload{
		Name:      randomProfileName(),
		Birthdate: randomBirthdate(),
	}
}

// randomProfileName 生成“首字母大写 + 若干小写字母”的随机姓名。
func randomProfileName() string {
	return randomCharset("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1) + randomCharset("abcdefghijklmnopqrstuvwxyz", randomInt(4, 8))
}

// randomBirthdate 把生日限制在稳定可接受的成年区间，减少无效风控噪声。
func randomBirthdate() string {
	year := randomInt(1985, 2002)
	month := randomInt(1, 12)
	day := randomInt(1, 28)
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}

// randomAlphaNumeric 生成指定长度的字母数字随机串。
func randomAlphaNumeric(length int) string {
	return randomCharset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", length)
}

// randomCharset 从给定字符集里均匀抽样，避免引入 math/rand 的可预测序列。
func randomCharset(charset string, length int) string {
	if length <= 0 || charset == "" {
		return ""
	}

	builder := strings.Builder{}
	builder.Grow(length)
	maxIndex := big.NewInt(int64(len(charset)))
	for i := 0; i < length; i++ {
		index, err := rand.Int(rand.Reader, maxIndex)
		if err != nil {
			panic(err)
		}
		builder.WriteByte(charset[index.Int64()])
	}
	return builder.String()
}

// randomInt 返回闭区间 [min, max] 的随机整数。
func randomInt(min, max int) int {
	if max <= min {
		return min
	}

	delta := big.NewInt(int64(max - min + 1))
	value, err := rand.Int(rand.Reader, delta)
	if err != nil {
		panic(err)
	}
	return min + int(value.Int64())
}

// randomUUID 生成 ext-oai-did 需要的 UUIDv4 字符串。
func randomUUID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}

	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		buffer[0:4],
		buffer[4:6],
		buffer[6:8],
		buffer[8:10],
		buffer[10:16],
	)
}
