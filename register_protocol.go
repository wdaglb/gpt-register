package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"go-register/utils"
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
	maxLeaseFailureStreak      = 5
)

// registrationAttemptResult 记录单个注册任务的持久化结果。
// Why: worker 并发执行时，只把摘要结果传回聚合层，避免直接在 goroutine 里竞争写文件。
type registrationAttemptResult struct {
	Email    string
	Password string
	Status   string
	Reason   string
	Err      error
	Stop     bool

	OAuthStatus string
	OAuthErr    error
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
func runRegister(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, authorizeInline bool) error {
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
				result := runRegisterAttempt(parent, cfg, mailClient, logger, store, ui, authorizeInline, workerID)
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
	authorizeSuccess := 0
	authorizeFail := 0
	var firstErr error
	var firstAuthorizeErr error
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
		if strings.TrimSpace(result.OAuthStatus) == "" {
			continue
		}
		if isOAuthSuccessful(result.OAuthStatus) {
			authorizeSuccess++
			continue
		}
		authorizeFail++
		logger.Printf("授权失败账号=%s status=%s err=%v", result.Email, result.OAuthStatus, result.OAuthErr)
		if firstAuthorizeErr == nil && result.OAuthErr != nil {
			firstAuthorizeErr = result.OAuthErr
		}
	}

	logger.Printf("注册任务结束: success=%d fail=%d authorize_success=%d authorize_fail=%d output=%s", success, fail, authorizeSuccess, authorizeFail, cfg.accountsFile)
	if success == 0 && fail > 0 {
		if firstErr != nil {
			return fmt.Errorf("全部注册失败: %w", firstErr)
		}
		return fmt.Errorf("全部注册失败")
	}
	if firstAuthorizeErr != nil {
		return fmt.Errorf("注册阶段已完成，但授权阶段存在失败: %w", firstAuthorizeErr)
	}
	return nil
}

// runRegisterUntilTargetSuccess 在 pipeline 模式下持续注册，直到注册成功数达到目标。
// Why: pipeline 需要保证授权线程最终拿到足够多的已注册账号；如果只按固定尝试次数结束，注册刚达标时授权侧可能还没收尾完成。
func runRegisterUntilTargetSuccess(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, authorizeInline bool) error {
	if cfg.count <= 0 {
		return fmt.Errorf("pipeline 模式下 count 必须大于 0")
	}
	if cfg.workers <= 0 {
		return fmt.Errorf("pipeline 模式下 workers 必须大于 0")
	}

	workerCount := cfg.workers
	if workerCount > cfg.count {
		workerCount = cfg.count
	}

	jobs := make(chan int, workerCount)
	results := make(chan registrationAttemptResult, workerCount)

	var workers sync.WaitGroup
	for workerID := 1; workerID <= workerCount; workerID++ {
		workerID := workerID
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range jobs {
				ui.RecordRegisterStart()
				result := runRegisterAttempt(parent, cfg, mailClient, logger, store, ui, authorizeInline, workerID)
				ui.RecordRegisterFinish(result.Status == "ok")
				results <- result
			}
		}()
	}

	success := 0
	fail := 0
	authorizeSuccess := 0
	authorizeFail := 0
	inFlight := 0
	nextJobID := 0
	leaseFailureStreak := 0
	stopRequested := false
	stopMessage := ""
	var firstErr error
	var firstAuthorizeErr error

	dispatchJob := func() {
		nextJobID++
		inFlight++
		jobs <- nextJobID
	}

	for inFlight < workerCount && shouldDispatchPipelineRegisterJob(cfg.count, success, inFlight) {
		dispatchJob()
	}

	for inFlight > 0 {
		result := <-results
		inFlight--

		switch result.Status {
		case "ok":
			success++
			leaseFailureStreak = 0
		default:
			fail++
			logger.Printf("注册失败账号=%s reason=%s err=%v", result.Email, result.Reason, result.Err)
			if firstErr == nil && result.Err != nil {
				firstErr = result.Err
			}
			if isLeaseFailureReason(result.Reason) {
				leaseFailureStreak++
				if leaseFailureStreak >= maxLeaseFailureStreak && !stopRequested {
					// Why: pipeline 在目标成功数未达标时会持续补发任务；连续租号失败说明邮箱池/服务侧已经不可用，不应无限打满循环。
					stopRequested = true
					stopMessage = fmt.Sprintf("连续租用邮箱失败达到 %d 次", leaseFailureStreak)
					logger.Printf("pipeline 注册阶段停止补发新任务: %s", stopMessage)
				}
			} else {
				leaseFailureStreak = 0
			}
			if result.Stop && !stopRequested {
				// Why: 邮箱池已空时继续补发注册任务只会产生相同失败，因此这里把它当作 pipeline 注册侧的停止信号。
				stopRequested = true
				stopMessage = "当前没有可用邮箱账号"
				logger.Printf("pipeline 注册阶段停止补发新任务: %s", stopMessage)
			}
		}
		if strings.TrimSpace(result.OAuthStatus) != "" {
			if isOAuthSuccessful(result.OAuthStatus) {
				authorizeSuccess++
			} else {
				authorizeFail++
				logger.Printf("授权失败账号=%s status=%s err=%v", result.Email, result.OAuthStatus, result.OAuthErr)
				if firstAuthorizeErr == nil && result.OAuthErr != nil {
					firstAuthorizeErr = result.OAuthErr
				}
			}
		}

		if parent.Err() == nil && !stopRequested {
			for inFlight < workerCount && shouldDispatchPipelineRegisterJob(cfg.count, success, inFlight) {
				dispatchJob()
			}
		}
	}

	close(jobs)
	workers.Wait()
	close(results)

	logger.Printf("注册任务结束: success=%d fail=%d authorize_success=%d authorize_fail=%d output=%s", success, fail, authorizeSuccess, authorizeFail, cfg.accountsFile)
	if success >= cfg.count {
		if firstAuthorizeErr != nil {
			return fmt.Errorf("注册已达到目标数量=%d，但授权阶段存在失败: %w", cfg.count, firstAuthorizeErr)
		}
		return nil
	}
	if stopRequested {
		if firstErr != nil {
			return fmt.Errorf("注册提前停止: success=%d target=%d reason=%s: %w", success, cfg.count, stopMessage, firstErr)
		}
		return fmt.Errorf("注册提前停止: success=%d target=%d reason=%s", success, cfg.count, stopMessage)
	}
	if parent.Err() != nil {
		if firstErr != nil {
			return fmt.Errorf("注册未达到目标数量: success=%d target=%d: %w；最后错误=%v", success, cfg.count, parent.Err(), firstErr)
		}
		return fmt.Errorf("注册未达到目标数量: success=%d target=%d: %w", success, cfg.count, parent.Err())
	}
	if success == 0 && fail > 0 {
		if firstErr != nil {
			return fmt.Errorf("全部注册失败: %w", firstErr)
		}
		return fmt.Errorf("全部注册失败")
	}
	if firstErr != nil {
		return fmt.Errorf("注册未达到目标数量: success=%d target=%d: %w", success, cfg.count, firstErr)
	}
	if firstAuthorizeErr != nil {
		return fmt.Errorf("注册未达到目标数量且授权存在失败: success=%d target=%d: %w", success, cfg.count, firstAuthorizeErr)
	}
	return fmt.Errorf("注册未达到目标数量: success=%d target=%d", success, cfg.count)
}

// shouldDispatchPipelineRegisterJob 判断 pipeline 是否需要继续补发新的注册任务。
// Why: 只要“已成功数 + 正在执行数”仍小于目标值，就应该继续补位；这样既能保持并发，也能避免目标达标后继续超发注册。
func shouldDispatchPipelineRegisterJob(targetSuccess, success, inFlight int) bool {
	return success+inFlight < targetSuccess
}

// isNoAvailableLeaseError 判断租号失败是否属于“邮箱池已空”。
// Why: 普通网络错误仍值得继续重试，但“当前没有可用邮箱账号”已经是明确的容量耗尽信号，应停止继续补注册。
func isNoAvailableLeaseError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "当前没有可用邮箱账号")
}

// isLeaseFailureReason 判断失败原因是否发生在邮箱租用阶段。
// Why: 只有租号环节连续失败才代表“无法拿到输入账号”，这时应停止 pipeline 补发，而不是把业务注册失败也计入同一阈值。
func isLeaseFailureReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == "lease_account" || reason == "lease_account_unavailable"
}

// runRegisterAttempt 负责单个邮箱租约的一次完整注册尝试。
// 核心流程：租号 -> 纯协议注册 -> 立即写入 accounts.txt -> 成功标记 used / 失败归还租约。
func runRegisterAttempt(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, authorizeInline bool, workerID int) registrationAttemptResult {
	cfg.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
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
		result.Stop = isNoAvailableLeaseError(err)
		if result.Stop {
			result.Reason = "lease_account_unavailable"
		}
		logger.Printf("[worker-%d] 租用邮箱失败: %v", workerID, err)
		return result
	}

	result.Email = lease.Email
	result.Password = generateRegistrationPassword()
	prefix := fmt.Sprintf("[worker-%d][%s]", workerID, lease.Email)
	logger.Printf("%s 已租到邮箱 account_id=%d", prefix, lease.ID)

	client, err := newProtocolClient(cfg, logger)
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
			registered, detectErr := client.detectRegisteredEmailAfterInvalidAuthStep(attemptCtx, cfg, lease.Email)
			if detectErr != nil {
				// Why: invalid_auth_step 本身不能稳定说明“邮箱已注册”；探测失败时宁可归还重试，也不要误把可用邮箱移出池子。
				logger.Printf("%s 注册密码阶段命中 invalid_auth_step，但判定邮箱注册状态失败，先归还租约: %v", prefix, detectErr)
				returnAccountWithCleanup(cfg, mailClient, lease, logger, prefix)
			} else if registered {
				// Why: 只有额外确认该邮箱已进入登录密码阶段时，才能认定它已注册成功，不应继续回到池里重复注册。
				markUsedWithCleanup(cfg, mailClient, lease, logger, prefix)
				logger.Printf("%s 注册密码阶段命中 invalid_auth_step，且已确认邮箱已注册，邮箱已移出池子", prefix)
			} else {
				logger.Printf("%s 注册密码阶段命中 invalid_auth_step，但未确认邮箱已注册，归还租约等待后续重试", prefix)
				returnAccountWithCleanup(cfg, mailClient, lease, logger, prefix)
			}
		} else {
			returnAccountWithCleanup(cfg, mailClient, lease, logger, prefix)
		}
		logger.Printf("%s 注册失败: %v", prefix, err)
		return result
	}

	result.Status = "ok"
	result.Reason = "ok"
	result.Err = nil
	record := accountRecord{
		Email:    result.Email,
		Password: result.Password,
	}
	if store != nil {
		updatedRecord, writeErr := store.upsertRegistration(result.Email, result.Password, "ok", time.Now())
		if writeErr != nil {
			result.Status = "fail"
			result.Reason = "accounts_write"
			result.Err = fmt.Errorf("注册成功但写入 accounts 失败: %w", writeErr)
			logger.Printf("%s 注册成功但写入 accounts 失败: %v", prefix, writeErr)
			return result
		}
		record = updatedRecord
	}
	markUsedWithCleanup(cfg, mailClient, lease, logger, prefix)
	logger.Printf("%s 注册成功", prefix)
	if authorizeInline {
		logger.Printf("%s 注册成功后继续复用同一链路执行授权", prefix)
		authorizationResult := authorizeAccountWithClient(attemptCtx, cfg, mailClient, logger, store, record, client, prefix)
		result.OAuthStatus = authorizationResult.OAuthStatus
		result.OAuthErr = authorizationResult.Err
		if ui != nil {
			ui.RecordAuthorizeFinish(isAuthorizationSuccessful(authorizationResult))
		}
	}
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
	code, err := mailClient.waitCodeByAccount(
		waitCtx,
		lease.ID,
		cfg.mailbox,
		cfg.pollInterval,
		time.Now().UTC().Add(-15*time.Second),
		func(elapsed time.Duration) {
			logger.Printf("%s 等待注册邮箱验证码 %s...", prefix, elapsed.Truncate(time.Second))
		},
	)
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
	params.Set("ext-oai-did", c.deviceID)
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
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, c, bootstrap, "username_password_create", registerStartPageURL)
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
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, c, bootstrap, "username_password_create", registerPasswordPageURL)
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
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, c, bootstrap, "oauth_create_account", registerAboutYouPageURL)
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
// Why: 只有“注册密码阶段 + invalid_auth_step”才会进入额外探测；是否真正移出池子，还要再确认邮箱是否已注册。
func shouldKeepRegisterLeaseOnInvalidAuthStep(err error) bool {
	flowErr, ok := asFlowResultError(err)
	if !ok || flowErr == nil || strings.TrimSpace(flowErr.reason) != "user_register" {
		return false
	}
	return strings.Contains(flowErr.Error(), "invalid_auth_step")
}

// detectRegisteredEmailAfterInvalidAuthStep 在注册密码阶段命中 invalid_auth_step 后，用登录入口确认邮箱是否已注册。
// Why: 同一个错误码既可能代表“邮箱已存在”，也可能只是当前注册状态机异常；只有看到登录链路进入密码页，才能把邮箱安全移出池子。
func (c *protocolClient) detectRegisteredEmailAfterInvalidAuthStep(ctx context.Context, cfg config, email string) (bool, error) {
	session := generateOAuthSession()
	bootstrap, err := c.bootstrapLoginPage(ctx, session.AuthURL)
	if err != nil {
		return false, fmt.Errorf("初始化登录探测页失败: %w", err)
	}

	identifierResult, err := c.submitIdentifier(ctx, cfg, bootstrap, email)
	if err != nil {
		return false, fmt.Errorf("登录探测提交邮箱失败: %w", err)
	}
	return isRegisteredEmailFromIdentifierResult(identifierResult), nil
}

// isRegisteredEmailFromIdentifierResult 根据登录入口提交邮箱后的响应判断账号是否已注册。
// Why: 已注册账号会继续推进到登录密码阶段；未进入密码页时，不能贸然认定该邮箱已存在。
func isRegisteredEmailFromIdentifierResult(result *httpResult) bool {
	if result == nil {
		return false
	}

	step := parseAuthAPIEnvelope(result.Body)
	if step.Page.Type == "login_password" {
		return true
	}

	for _, candidate := range []string{
		step.ContinueURL,
		step.Page.Payload.URL,
		result.FinalURL,
		result.Location,
	} {
		lowerCandidate := strings.ToLower(strings.TrimSpace(candidate))
		if lowerCandidate == "" {
			continue
		}
		if strings.Contains(lowerCandidate, "/log-in/password") || strings.Contains(lowerCandidate, "/u/login/password") {
			return true
		}
	}

	return false
}

// returnAccountWithCleanup 在主流程失败后归还邮箱租约。
func returnAccountWithCleanup(cfg config, mailClient *webMailClient, lease *webMailAccount, logger *log.Logger, prefix string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout(cfg))
	defer cancel()

	// Why: 主流程上下文可能已经因超时取消，归还租约必须脱离主链路单独兜底，否则邮箱会长期卡在 leased 状态。
	if err := utils.RunWithWaitLog(cleanupCtx, utils.WaitLogFunc(logger.Printf, prefix+" 归还邮箱"), func() error {
		return mailClient.returnAccount(cleanupCtx, lease.ID, lease.LeaseToken)
	}); err != nil {
		logger.Printf("%s 归还邮箱失败: %v", prefix, err)
	}
}

// markUsedWithCleanup 在注册成功后把邮箱标记为已使用，避免再次被租出。
func markUsedWithCleanup(cfg config, mailClient *webMailClient, lease *webMailAccount, logger *log.Logger, prefix string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout(cfg))
	defer cancel()

	if err := utils.RunWithWaitLog(cleanupCtx, utils.WaitLogFunc(logger.Printf, prefix+" 标记邮箱已使用"), func() error {
		return mailClient.markUsed(cleanupCtx, lease.ID, lease.LeaseToken)
	}); err != nil {
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

// enqueueAuthorizeJob 在 pipeline 模式下把新注册账号投递给授权 worker。
// Why: 当授权侧暂时处理不过来时，这里是注册线程最容易“看起来卡死”的点，因此需要显式打印等待日志。
func enqueueAuthorizeJob(ctx context.Context, logger *log.Logger, prefix string, jobs chan<- accountRecord, record accountRecord) bool {
	waitTicker := time.NewTicker(time.Second)
	defer waitTicker.Stop()

	waited := time.Duration(0)
	for {
		select {
		case jobs <- record:
			return true
		case <-ctx.Done():
			logger.Printf("%s 等待授权队列时中断: %v", prefix, ctx.Err())
			return false
		case <-waitTicker.C:
			waited += time.Second
			logger.Printf("%s 等待授权队列 %s...", prefix, waited)
		}
	}
}

// generateRegistrationPassword 生成与参考脚本兼容的注册密码格式。
func generateRegistrationPassword() string {
	return utils.RandomAlphaNumeric(10) + "aA1!"
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
	return utils.RandomCharset("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1) + utils.RandomCharset("abcdefghijklmnopqrstuvwxyz", utils.RandomInt(4, 8))
}

// randomBirthdate 把生日限制在稳定可接受的成年区间，减少无效风控噪声。
func randomBirthdate() string {
	year := utils.RandomInt(1985, 2002)
	month := utils.RandomInt(1, 12)
	day := utils.RandomInt(1, 28)
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
}
