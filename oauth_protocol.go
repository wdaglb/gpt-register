package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	stdhttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/net/html"
	sentinel "openai-sentinel-go"
)

const (
	auth0BaseURL          = "https://auth0.openai.com"
	authAuthorizeResume   = auth0BaseURL + "/authorize/resume?state=%s"
	authLoginIdentifier   = auth0BaseURL + "/u/login/identifier?state=%s"
	authLoginPassword     = auth0BaseURL + "/u/login/password?state=%s"
	auth0AcceptHeader     = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	defaultOAuthUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
	oauthAuthorizeURL     = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL         = "https://auth.openai.com/oauth/token"
	oauthClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthRedirectURI      = "http://localhost:1455/auth/callback"
	chromeSecCHUA         = `"Google Chrome";v="135", "Chromium";v="135", "Not.A/Brand";v="24"`
	chromeSecCHUAFull     = `"Google Chrome";v="135.0.0.0", "Chromium";v="135.0.0.0", "Not.A/Brand";v="24.0.0.0"`
)

var (
	statePattern      = regexp.MustCompile(`state=([^&"'>]+)`)
	stateValuePattern = regexp.MustCompile(`name=["']state["'][^>]*value=["']([^"']+)["']`)
	deviceIDPattern   = regexp.MustCompile(`"DeviceId":"([^"]+)"`)
)

// protocolLoginResult 汇总协议登录链路产出的关键结果。
// Why: callback/code/state/auth 文件是不同阶段消费的结果，收拢成结构体后更便于分阶段推进和补齐后续 consent 流。
type protocolLoginResult struct {
	CallbackURL  string
	Code         string
	State        string
	AuthFilePath string
}

// flowResultError 给底层协议错误额外挂上稳定的 reason。
// Why: 上层需要把失败原因落盘为可筛查字段，不能只依赖易变的原始 HTTP 文本。
type flowResultError struct {
	reason string
	err    error
}

// protocolClient 维护 TLS 指纹客户端和统一 UA。
// Why: 当前登录/注册链路都强依赖浏览器指纹一致性，把底层客户端放在这里可以避免每一步请求各自漂移。
type protocolClient struct {
	httpClient tls_client.HttpClient
	userAgent  string
}

type httpResult struct {
	StatusCode int
	Location   string
	FinalURL   string
	Body       string
	Header     http.Header
}

type requestOptions struct {
	Method        string
	URL           string
	AllowRedirect bool
	Accept        string
	ContentType   string
	Referer       string
	Origin        string
	Profile       requestProfile
	ExtraHeaders  map[string]string
	Form          url.Values
	Body          io.Reader
}

type requestProfile string

const (
	profileNavigate requestProfile = "navigate"
	profileForm     requestProfile = "form"
	profileAPI      requestProfile = "api"
	profileToken    requestProfile = "token"
)

type oauthSession struct {
	AuthURL  string
	State    string
	Verifier string
}

type bootstrapPage struct {
	FinalURL             string
	HTML                 string
	OpenAIClientID       string
	AppNameEnum          string
	AuthSessionID        string
	AuthSessionLoggingID string
	DeviceID             string
}

type oauthTokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// newFlowResultError 创建带 reason 的包装错误，便于调用方统一落盘。
func newFlowResultError(reason string, err error) error {
	return &flowResultError{
		reason: reason,
		err:    err,
	}
}

func (e *flowResultError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *flowResultError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func asFlowResultError(err error) (*flowResultError, bool) {
	var target *flowResultError
	if err == nil {
		return nil, false
	}
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

type authAPIEnvelope struct {
	ContinueURL string `json:"continue_url"`
	Method      string `json:"method"`
	Page        struct {
		Type    string `json:"type"`
		Payload struct {
			URL string `json:"url"`
		} `json:"payload"`
	} `json:"page"`
}

type htmlForm struct {
	ActionURL string
	Method    string
	Values    url.Values
}

// loginWithProtocol 执行已有账号的纯协议 OAuth 登录闭环。
// 核心流程：初始化前端会话 -> 提交邮箱/密码 -> 处理邮箱 OTP -> 交换 token -> 生成本地 auth 文件。
func loginWithProtocol(parent context.Context, cfg config, account loginAccount, mailClient *webMailClient, logger *log.Logger) (*protocolLoginResult, error) {
	httpClient, err := newProtocolClient(cfg)
	if err != nil {
		return nil, err
	}

	session := generateOAuthSession()
	logger.Printf("已生成 OAuth state: %s", trimForLog(session.State))

	logger.Print("开始协议登录 OpenAI OAuth")
	result, err := httpClient.completeOAuth(parent, cfg, account, mailClient, logger, session)
	if err != nil {
		return nil, err
	}

	logger.Print("使用 authorization code 交换 token")
	tokenResponse, err := httpClient.exchangeCode(parent, result.Code, session.Verifier)
	if err != nil {
		return nil, fmt.Errorf("token 交换失败: %w", err)
	}

	logger.Print("生成本地授权文件")
	authFilePath, err := writeAuthFile(cfg.authDir, account, tokenResponse)
	if err != nil {
		return nil, err
	}
	result.AuthFilePath = authFilePath
	return result, nil
}

// newProtocolClient 创建带 Chrome TLS 指纹和共享 Cookie Jar 的客户端。
// Why: OpenAI 当前链路对 TLS 指纹、Cookie 连续性都较敏感，因此这里显式模拟同一浏览器会话。
func newProtocolClient(cfg config) (*protocolClient, error) {
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(int(cfg.requestTimeout.Seconds())),
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithCookieJar(jar),
		tls_client.WithRandomTLSExtensionOrder(),
	}
	if strings.TrimSpace(cfg.proxy) != "" {
		options = append(options, tls_client.WithProxyUrl(cfg.proxy))
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("创建 TLS 指纹客户端失败: %w", err)
	}

	return &protocolClient{
		httpClient: client,
		userAgent:  defaultOAuthUserAgent,
	}, nil
}

// generateOAuthSession 生成 OAuth authorize URL 以及 PKCE 所需 state/verifier。
// Why: CLI 登录必须带上与 authorize URL 一一对应的 code_verifier，否则最后的 token 交换会失败。
func generateOAuthSession() oauthSession {
	state := randomURLToken(16)
	verifier := randomURLToken(64)
	challenge := sha256Base64URL(verifier)

	params := url.Values{}
	params.Set("client_id", oauthClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", oauthRedirectURI)
	params.Set("scope", "openid email profile offline_access")
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("prompt", "login")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")

	return oauthSession{
		AuthURL:  oauthAuthorizeURL + "?" + params.Encode(),
		State:    state,
		Verifier: verifier,
	}
}

// completeOAuth 推进整条纯协议 OAuth 登录链路，直到拿到 localhost callback。
// Note: 对于落入 add_phone 的账号，这里仍会按业务要求直接失败返回，而不会尝试绕过手机验证。
func (c *protocolClient) completeOAuth(ctx context.Context, cfg config, account loginAccount, mailClient *webMailClient, logger *log.Logger, session oauthSession) (*protocolLoginResult, error) {
	bootstrap, err := c.bootstrapLoginPage(ctx, session.AuthURL)
	if err != nil {
		return nil, err
	}
	logger.Printf("已初始化登录页: %s", bootstrap.FinalURL)

	identifierResult, err := c.submitIdentifier(ctx, cfg, bootstrap, account.email)
	if err != nil {
		return nil, err
	}
	identifierStep := parseAuthAPIEnvelope(identifierResult.Body)
	logger.Printf("邮箱阶段返回: status=%d next=%s", identifierResult.StatusCode, identifierStep.Page.Type)

	passwordPageURL := identifierStep.ContinueURL
	if passwordPageURL == "" {
		passwordPageURL = bootstrap.FinalURL + "/password"
	}
	if err := c.openPage(ctx, passwordPageURL, bootstrap.FinalURL); err != nil {
		return nil, fmt.Errorf("打开密码页失败: %w", err)
	}

	passwordResult, err := c.submitPassword(ctx, cfg, bootstrap, account.password)
	if err != nil {
		return nil, err
	}
	passwordStep := parseAuthAPIEnvelope(passwordResult.Body)
	logger.Printf("密码阶段返回: status=%d next=%s", passwordResult.StatusCode, passwordStep.Page.Type)

	currentStep := passwordStep

	if passwordStep.Page.Type == "email_otp_verification" || needsEmailOTP(passwordStep.ContinueURL) || needsEmailOTP(passwordResult.FinalURL) || needsEmailOTP(passwordResult.Location) {
		logger.Print("检测到邮箱 OTP 挑战，准备收码验证")
		emailVerificationURL := passwordStep.ContinueURL
		if emailVerificationURL == "" {
			emailVerificationURL = "https://auth.openai.com/email-verification"
		}
		if err := c.openPage(ctx, emailVerificationURL, passwordPageURL); err != nil {
			return nil, fmt.Errorf("打开邮箱验证码页失败: %w", err)
		}
		if err := c.sendEmailOTP(ctx, logger); err != nil {
			logger.Printf("发送 OTP 接口未确认成功，继续尝试收码: %v", err)
		}

		waitCtx, cancel := context.WithTimeout(ctx, cfg.otpTimeout)
		defer cancel()

		// Why: 邮件服务的时间戳精度只有秒，而本地 time.Now() 带毫秒；给一个回看窗口，避免刚发出的 OTP 被误判为“旧邮件”。
		code, err := mailClient.waitCodeByEmail(waitCtx, account.email, cfg.mailbox, cfg.pollInterval, time.Now().UTC().Add(-15*time.Second))
		if err != nil {
			return nil, fmt.Errorf("等待邮箱验证码失败: %w", err)
		}
		logger.Print("收到验证码，准备提交")
		validateResult, err := c.validateEmailOTP(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("提交邮箱验证码失败: %w", err)
		}
		validateStep := parseAuthAPIEnvelope(validateResult.Body)
		logger.Printf("OTP 阶段返回: status=%d next=%s", validateResult.StatusCode, validateStep.Page.Type)
		if validateStep.Page.Type == "add_phone" {
			return nil, newFlowResultError("add_phone", fmt.Errorf("当前账号在邮箱验证码后进入 add_phone，必须绑定手机后才能继续"))
		}
		currentStep = validateStep
	}

	result, err := c.finalizeOAuth(ctx, logger, session, currentStep)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *protocolClient) exchangeCode(ctx context.Context, code, verifier string) (*oauthTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("redirect_uri", oauthRedirectURI)
	form.Set("code_verifier", verifier)

	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           oauthTokenURL,
		AllowRedirect: true,
		Accept:        "application/json",
		ContentType:   "application/x-www-form-urlencoded",
		Origin:        "https://auth.openai.com",
		Referer:       oauthAuthorizeURL,
		Profile:       profileToken,
		Form:          form,
	})
	if err != nil {
		return nil, err
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d %s", result.StatusCode, limitBody(result.Body))
	}

	response := oauthTokenResponse{}
	if err := json.Unmarshal([]byte(result.Body), &response); err != nil {
		return nil, err
	}
	if response.AccessToken == "" || response.IDToken == "" {
		return nil, fmt.Errorf("token 响应缺少 access_token/id_token")
	}
	return &response, nil
}

// writeAuthFile 把 token 响应转换为本地 Codex 可消费的 auth JSON。
func writeAuthFile(authDir string, account loginAccount, tokenResponse *oauthTokenResponse) (string, error) {
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 auth 目录失败: %w", err)
	}

	payload := buildCodexAuthJSON(tokenResponse, account.email, account.password)
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化授权文件失败: %w", err)
	}

	filePath := filepath.Join(authDir, "codex-"+sanitizeFilename(account.email)+".json")
	if err := os.WriteFile(filePath, body, 0o644); err != nil {
		return "", fmt.Errorf("写入授权文件失败: %w", err)
	}
	return filePath, nil
}

// buildCodexAuthJSON 构造与本地工具链兼容的授权文件结构。
// Why: 这里额外保留 email/password，是为了让后续批量调试和结果回放时不需要再去额外关联账号源文件。
func buildCodexAuthJSON(tokenResponse *oauthTokenResponse, email, password string) map[string]any {
	claims := decodeJWTPayload(tokenResponse.IDToken)
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	accountID, _ := authClaims["chatgpt_account_id"].(string)
	claimEmail, _ := claims["email"].(string)
	if claimEmail == "" {
		claimEmail = email
	}

	now := time.Now().UTC()
	return map[string]any{
		"id_token":      tokenResponse.IDToken,
		"access_token":  tokenResponse.AccessToken,
		"refresh_token": tokenResponse.RefreshToken,
		"account_id":    accountID,
		"last_refresh":  now.Format(time.RFC3339),
		"email":         claimEmail,
		"type":          "codex",
		"expired":       now.Add(time.Duration(tokenResponse.ExpiresIn) * time.Second).Format(time.RFC3339),
		"password":      password,
	}
}

// decodeJWTPayload 仅做无校验载荷解析，用于提取 email 和 account_id 等展示字段。
func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)

	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}

	claims := map[string]any{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return map[string]any{}
	}
	return claims
}

func sha256Base64URL(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLToken(byteLength int) string {
	if byteLength <= 0 {
		return ""
	}

	buffer := make([]byte, byteLength)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

// buildSentinelHeader 为指定 flow 生成 OpenAI-Sentinel-Token 请求头。
// Why: 当前登录页和密码页都由 Sentinel SDK 动态保护，缺少这个头通常会直接返回前端状态机错误。
func buildSentinelHeader(ctx context.Context, cfg config, bootstrap *bootstrapPage, flow, referer string) (map[string]string, error) {
	session, err := newSentinelSession(cfg, bootstrap)
	if err != nil {
		return nil, err
	}

	service := sentinel.NewService(sentinel.Config{
		SentinelBaseURL:     "https://sentinel.openai.com",
		SentinelTimeout:     cfg.requestTimeout,
		SentinelMaxAttempts: 2,
	})
	token, err := service.Build(ctx, session, flow, referer, "")
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(token)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"OpenAI-Sentinel-Token": string(encoded),
	}, nil
}

// newSentinelSession 构造纯 Go Sentinel 求解所需的浏览器样本。
// Why: DeviceID、语言和屏幕信息要尽量与登录页初始化阶段保持一致，减少风控侧看到的前后指纹漂移。
func newSentinelSession(cfg config, bootstrap *bootstrapPage) (*sentinel.Session, error) {
	transport := &stdhttp.Transport{}
	if strings.TrimSpace(cfg.proxy) != "" {
		proxyURL, err := url.Parse(cfg.proxy)
		if err != nil {
			return nil, fmt.Errorf("解析 Sentinel 代理失败: %w", err)
		}
		transport.Proxy = stdhttp.ProxyURL(proxyURL)
	}

	return &sentinel.Session{
		Client:              &stdhttp.Client{Timeout: cfg.requestTimeout, Transport: transport},
		DeviceID:            bootstrap.DeviceID,
		UserAgent:           defaultOAuthUserAgent,
		ScreenWidth:         1512,
		ScreenHeight:        982,
		HeapLimit:           4294705152,
		HardwareConcurrency: 8,
		Language:            "zh-CN",
		LanguagesJoin:       "zh-CN,en-US",
		Persona: sentinel.Persona{
			Platform:              "MacIntel",
			Vendor:                "Google Inc.",
			RequirementsScriptURL: "https://sentinel.openai.com/backend-api/sentinel/sdk.js",
		},
	}, nil
}

// parseAuthAPIEnvelope 优先按 JSON 解析 next-step 响应，失败时再退化为关键词判断。
// Why: 线上链路偶尔会返回非标准 JSON 或 HTML 片段，完全依赖严格 JSON 解析会让调试信息丢失。
func parseAuthAPIEnvelope(body string) authAPIEnvelope {
	envelope := authAPIEnvelope{}
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		return envelope
	}

	switch {
	case strings.Contains(body, "login_password"):
		envelope.Page.Type = "login_password"
	case strings.Contains(body, "email_otp_verification"):
		envelope.Page.Type = "email_otp_verification"
	case strings.Contains(body, "sign_in_with_chatgpt_codex_consent"):
		envelope.Page.Type = "sign_in_with_chatgpt_codex_consent"
	case strings.Contains(body, "workspace"):
		envelope.Page.Type = "workspace"
	default:
		envelope.Page.Type = "unknown"
	}
	return envelope
}

// finalizeOAuth 处理 OTP 之后的 consent / callback 阶段，直到拿到 localhost 回调。
// Why: 无需 add_phone 的账号后半段不再是邮箱密码输入，而是 consent 点击和 302 跳转，因此单独抽成收尾阶段更清晰。
func (c *protocolClient) finalizeOAuth(ctx context.Context, logger *log.Logger, session oauthSession, step authAPIEnvelope) (*protocolLoginResult, error) {
	if callbackURL := extractCallbackURLFromEnvelope(step); callbackURL != "" {
		return buildProtocolLoginResultFromCallback(callbackURL, session.State)
	}
	if step.Page.Type == "add_phone" {
		return nil, newFlowResultError("add_phone", fmt.Errorf("当前账号需要绑定手机后才能继续授权"))
	}

	nextURL := firstNonEmpty(step.Page.Payload.URL, step.ContinueURL)
	switch {
	case step.Page.Type == "sign_in_with_chatgpt_codex_consent" || strings.Contains(strings.ToLower(nextURL), "consent"):
		logger.Print("检测到 consent 阶段，准备协议点击同意")
		callbackURL, err := c.confirmConsentAndCaptureCallback(ctx, session.State, nextURL)
		if err != nil {
			return nil, newFlowResultError("oauth_consent", err)
		}
		return buildProtocolLoginResultFromCallback(callbackURL, session.State)
	case nextURL != "":
		if strings.Contains(nextURL, "localhost:1455") {
			return buildProtocolLoginResultFromCallback(nextURL, session.State)
		}
	}

	logger.Print("尝试恢复 OAuth 会话并捕获 localhost callback")
	callbackURL, err := c.resumeAndCaptureCallback(ctx, session.State)
	if err != nil {
		return nil, newFlowResultError("oauth_callback", err)
	}
	return buildProtocolLoginResultFromCallback(callbackURL, session.State)
}

func extractCallbackURLFromEnvelope(step authAPIEnvelope) string {
	for _, candidate := range []string{step.Page.Payload.URL, step.ContinueURL} {
		if strings.Contains(candidate, "localhost:1455") {
			return candidate
		}
	}
	return ""
}

func buildProtocolLoginResultFromCallback(callbackURL, expectedState string) (*protocolLoginResult, error) {
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		return nil, fmt.Errorf("解析 callback URL 失败: %w", err)
	}

	query := parsed.Query()
	code := query.Get("code")
	state := query.Get("state")
	if code == "" || state == "" {
		return nil, fmt.Errorf("callback 缺少 code/state: %s", callbackURL)
	}
	if expectedState != "" && state != expectedState {
		return nil, fmt.Errorf("callback state 不匹配: got=%s want=%s", state, expectedState)
	}

	return &protocolLoginResult{
		CallbackURL: callbackURL,
		Code:        code,
		State:       state,
	}, nil
}

// confirmConsentAndCaptureCallback 以纯协议方式打开 consent 页、提交表单，并抓取最终 callback。
func (c *protocolClient) confirmConsentAndCaptureCallback(ctx context.Context, state, consentURL string) (string, error) {
	if strings.TrimSpace(consentURL) == "" {
		return c.resumeAndCaptureCallback(ctx, state)
	}

	pageResult, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           consentURL,
		AllowRedirect: true,
		Accept:        auth0AcceptHeader,
		Referer:       auth0BaseURL,
		Profile:       profileNavigate,
	})
	if err != nil {
		return "", fmt.Errorf("打开 consent 页面失败: %w", err)
	}

	if callbackURL := extractCallbackURLFromHTTPResult(pageResult); callbackURL != "" {
		return callbackURL, nil
	}

	form, err := extractFirstHTMLForm(pageResult.Body, pageResult.FinalURL)
	if err != nil {
		if stateFromPage := extractState(pageResult.Body); stateFromPage != "" {
			return c.resumeAndCaptureCallback(ctx, stateFromPage)
		}
		return c.resumeAndCaptureCallback(ctx, state)
	}

	submitResult, err := c.submitHTMLForm(ctx, form, pageResult.FinalURL)
	if err != nil {
		return "", fmt.Errorf("提交 consent 表单失败: %w", err)
	}

	if callbackURL := extractCallbackURLFromHTTPResult(submitResult); callbackURL != "" {
		return callbackURL, nil
	}

	if nextState := firstNonEmpty(extractState(submitResult.Location), extractState(submitResult.FinalURL), extractState(submitResult.Body), state); nextState != "" {
		return c.resumeAndCaptureCallback(ctx, nextState)
	}
	return "", fmt.Errorf("consent 提交后未捕获到 callback")
}

func (c *protocolClient) submitHTMLForm(ctx context.Context, form *htmlForm, referer string) (*httpResult, error) {
	if form == nil {
		return nil, fmt.Errorf("consent 表单为空")
	}

	method := strings.ToUpper(strings.TrimSpace(form.Method))
	if method == "" {
		method = http.MethodPost
	}

	options := requestOptions{
		Method:        method,
		URL:           form.ActionURL,
		AllowRedirect: false,
		Accept:        auth0AcceptHeader,
		Referer:       referer,
		Profile:       profileForm,
	}

	if origin := originFromURL(form.ActionURL); origin != "" {
		options.Origin = origin
	}

	switch method {
	case http.MethodGet:
		targetURL, err := url.Parse(form.ActionURL)
		if err != nil {
			return nil, fmt.Errorf("解析 consent GET action 失败: %w", err)
		}
		query := targetURL.Query()
		for key, values := range form.Values {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		targetURL.RawQuery = query.Encode()
		options.URL = targetURL.String()
		options.Profile = profileNavigate
	default:
		options.ContentType = "application/x-www-form-urlencoded"
		options.Form = form.Values
	}

	return c.doRequest(ctx, options)
}

func extractFirstHTMLForm(rawHTML, baseURL string) (*htmlForm, error) {
	document, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("解析 consent HTML 失败: %w", err)
	}

	formNode := findFirstNode(document, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.Data == "form"
	})
	if formNode == nil {
		return nil, fmt.Errorf("页面中没有 form")
	}

	method := strings.ToUpper(attributeValue(formNode, "method"))
	if method == "" {
		method = http.MethodPost
	}

	actionURL := resolveRelativeURL(baseURL, attributeValue(formNode, "action"))
	values := url.Values{}
	collectFormValues(formNode, values)

	return &htmlForm{
		ActionURL: actionURL,
		Method:    method,
		Values:    values,
	}, nil
}

func collectFormValues(root *html.Node, values url.Values) {
	if root == nil {
		return
	}

	if root.Type == html.ElementNode {
		switch root.Data {
		case "input":
			name := attributeValue(root, "name")
			if name == "" || attributeExists(root, "disabled") {
				break
			}
			inputType := strings.ToLower(attributeValue(root, "type"))
			if inputType == "checkbox" || inputType == "radio" {
				if !attributeExistsWithValue(root, "checked", "") {
					break
				}
			}
			values.Add(name, attributeValue(root, "value"))
		case "button":
			name := attributeValue(root, "name")
			if name != "" && !attributeExists(root, "disabled") {
				values.Add(name, attributeValue(root, "value"))
			}
		}
	}

	for child := root.FirstChild; child != nil; child = child.NextSibling {
		collectFormValues(child, values)
	}
}

func findFirstNode(root *html.Node, matcher func(*html.Node) bool) *html.Node {
	if root == nil {
		return nil
	}
	if matcher(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if matched := findFirstNode(child, matcher); matched != nil {
			return matched
		}
	}
	return nil
}

func attributeValue(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func attributeExists(node *html.Node, key string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return true
		}
	}
	return false
}

func attributeExistsWithValue(node *html.Node, key, value string) bool {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			if value == "" || attr.Val == value {
				return true
			}
		}
	}
	return false
}

func resolveRelativeURL(baseURL, ref string) string {
	if strings.TrimSpace(ref) == "" {
		return baseURL
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return ref
	}
	target, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(target).String()
}

func originFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func extractCallbackURLFromHTTPResult(result *httpResult) string {
	if result == nil {
		return ""
	}
	for _, candidate := range []string{result.Location, result.FinalURL, extractState(result.Body)} {
		if strings.Contains(candidate, "localhost:1455") {
			return candidate
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func extractDeviceID(body string) string {
	matches := deviceIDPattern.FindStringSubmatch(body)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

func extractBootstrapField(body, field string) string {
	pattern := regexp.MustCompile(fmt.Sprintf(`"%s":"([^"]+)"`, regexp.QuoteMeta(field)))
	matches := pattern.FindStringSubmatch(body)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

func writeDebugResponse(prefix, body string) {
	if prefix == "" {
		return
	}
	_ = os.MkdirAll("artifacts", 0o755)
	_ = os.WriteFile(filepath.Join("artifacts", prefix+".json"), []byte(body), 0o644)
}

func writeDebugHTTPResult(prefix string, result *httpResult) {
	if prefix == "" || result == nil {
		return
	}

	payload := map[string]any{
		"status_code": result.StatusCode,
		"location":    result.Location,
		"final_url":   result.FinalURL,
		"body":        result.Body,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}

	_ = os.MkdirAll("artifacts", 0o755)
	_ = os.WriteFile(filepath.Join("artifacts", prefix+".json"), encoded, 0o644)
}

// bootstrapLoginPage 初始化 OAuth 登录页，并提取前端会话所需的 bootstrap 字段。
// Why: 后续 Sentinel 求解和登录接口都依赖这里返回的 DeviceID / session 信息，必须先证明“当前真正渲染的页面”长什么样。
func (c *protocolClient) bootstrapLoginPage(ctx context.Context, authURL string) (*bootstrapPage, error) {
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           authURL,
		AllowRedirect: true,
		Accept:        auth0AcceptHeader,
		Profile:       profileNavigate,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化授权页失败: %w", err)
	}
	if strings.EqualFold(result.Header.Get("cf-mitigated"), "challenge") {
		return nil, fmt.Errorf("授权入口被 Cloudflare challenge 拦截")
	}

	deviceID := extractDeviceID(result.Body)
	if deviceID == "" {
		deviceID = randomURLToken(16)
	}

	page := &bootstrapPage{
		FinalURL:             result.FinalURL,
		HTML:                 result.Body,
		OpenAIClientID:       oauthClientID,
		AppNameEnum:          "oaicli",
		AuthSessionID:        extractBootstrapField(result.Body, "session_id"),
		AuthSessionLoggingID: extractBootstrapField(result.Body, "auth_session_logging_id"),
		DeviceID:             deviceID,
	}

	if page.FinalURL == "" {
		_ = os.WriteFile("debug_bootstrap.html", []byte(result.Body), 0o644)
		return nil, fmt.Errorf("授权入口未返回最终页面地址")
	}
	return page, nil
}

// openPage 用浏览器导航语义打开某个中间页，主要用于让服务端继续推进当前 auth session。
func (c *protocolClient) openPage(ctx context.Context, pageURL, referer string) error {
	_, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodGet,
		URL:           pageURL,
		AllowRedirect: true,
		Accept:        auth0AcceptHeader,
		Referer:       referer,
		Profile:       profileNavigate,
	})
	return err
}

// submitIdentifier 提交邮箱，推进到密码页。
func (c *protocolClient) submitIdentifier(ctx context.Context, cfg config, bootstrap *bootstrapPage, email string) (*httpResult, error) {
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, bootstrap, "authorize_continue", bootstrap.FinalURL)
	if err != nil {
		return nil, fmt.Errorf("生成 authorize_continue sentinel 失败: %w", err)
	}

	payload := strings.NewReader(fmt.Sprintf(`{"username":{"kind":"email","value":%q}}`, email))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           "https://auth.openai.com/api/accounts/authorize/continue",
		AllowRedirect: true,
		Referer:       bootstrap.FinalURL,
		Origin:        "https://auth.openai.com",
		Accept:        "application/json",
		ContentType:   "application/json",
		Profile:       profileAPI,
		Body:          payload,
		ExtraHeaders:  sentinelHeader,
	})
	if err != nil {
		return nil, fmt.Errorf("提交邮箱失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("authorize_continue", result.Body)
		return nil, fmt.Errorf("提交邮箱返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("authorize_continue_success", result)
	return result, nil
}

// submitPassword 提交密码，推进到 OTP 或 consent 下一步。
func (c *protocolClient) submitPassword(ctx context.Context, cfg config, bootstrap *bootstrapPage, password string) (*httpResult, error) {
	sentinelHeader, err := buildSentinelHeader(ctx, cfg, bootstrap, "password_verify", "https://auth.openai.com/log-in/password")
	if err != nil {
		return nil, fmt.Errorf("生成 password_verify sentinel 失败: %w", err)
	}

	payload := strings.NewReader(fmt.Sprintf(`{"password":%q}`, password))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           "https://auth.openai.com/api/accounts/password/verify",
		AllowRedirect: true,
		Referer:       "https://auth.openai.com/log-in/password",
		Origin:        "https://auth.openai.com",
		Accept:        "application/json",
		ContentType:   "application/json",
		Profile:       profileAPI,
		Body:          payload,
		ExtraHeaders:  sentinelHeader,
	})
	if err != nil {
		return nil, fmt.Errorf("提交密码失败: %w", err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		writeDebugResponse("password_verify", result.Body)
		return nil, fmt.Errorf("提交密码返回异常状态: %d body=%s", result.StatusCode, limitBody(result.Body))
	}
	writeDebugHTTPResult("password_verify_success", result)
	return result, nil
}

// sendEmailOTP 依次尝试当前前端链路和历史兼容链路的 OTP 发送接口。
// Why: OpenAI 曾多次调整发送验证码的入口，这里做多接口回退可以避免因为单个入口下线就整条链路失效。
func (c *protocolClient) sendEmailOTP(ctx context.Context, logger *log.Logger) error {
	sendRequests := []requestOptions{
		{
			Method:        http.MethodPost,
			URL:           "https://auth.openai.com/api/accounts/email-otp/resend",
			AllowRedirect: true,
			Accept:        "application/json",
			Referer:       "https://auth.openai.com/mfa-challenge/email-otp",
			Origin:        "https://auth.openai.com",
			Profile:       profileAPI,
			Body:          strings.NewReader(`{}`),
		},
		{
			Method:        http.MethodGet,
			URL:           "https://auth.openai.com/api/accounts/email-otp/send",
			AllowRedirect: true,
			Accept:        "application/json",
			Referer:       "https://auth.openai.com/mfa-challenge/email-otp",
			Origin:        "https://auth.openai.com",
			Profile:       profileAPI,
		},
		{
			Method:        http.MethodPost,
			URL:           "https://api.openai.com/dashboard/onboarding/email-otp/send",
			AllowRedirect: true,
			Accept:        "application/json",
			ContentType:   "application/json",
			Origin:        "https://platform.openai.com",
			Profile:       profileAPI,
			Body:          strings.NewReader(`{}`),
		},
	}

	var lastErr error
	for _, request := range sendRequests {
		result, err := c.doRequest(ctx, request)
		if err == nil && result.StatusCode >= 200 && result.StatusCode < 300 {
			logger.Printf("OTP 发送接口已触发: %s", request.URL)
			return nil
		}
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = fmt.Errorf("status=%d body=%s", result.StatusCode, limitBody(result.Body))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 OTP 发送接口")
	}
	return lastErr
}

// validateEmailOTP 提交邮箱验证码，并兼容历史 onboarding 接口。
// Note: 先走 auth.openai.com 主链路，只有失败时才回退旧平台接口，避免优先命中已经废弃的旧入口。
func (c *protocolClient) validateEmailOTP(ctx context.Context, code string) (*httpResult, error) {
	payload := strings.NewReader(fmt.Sprintf(`{"code":%q}`, code))
	result, err := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           "https://auth.openai.com/api/accounts/email-otp/validate",
		AllowRedirect: true,
		Accept:        "application/json",
		ContentType:   "application/json",
		Referer:       "https://auth.openai.com/mfa-challenge/email-otp",
		Origin:        "https://auth.openai.com",
		Profile:       profileAPI,
		Body:          payload,
	})
	if err == nil && result.StatusCode >= 200 && result.StatusCode < 300 {
		writeDebugHTTPResult("email_otp_validate_success", result)
		return result, nil
	}

	legacyPayload := strings.NewReader(fmt.Sprintf(`{"code":%q}`, code))
	result, legacyErr := c.doRequest(ctx, requestOptions{
		Method:        http.MethodPost,
		URL:           "https://api.openai.com/dashboard/onboarding/email-otp/validate",
		AllowRedirect: true,
		Accept:        "application/json",
		ContentType:   "application/json",
		Origin:        "https://platform.openai.com",
		Profile:       profileAPI,
		Body:          legacyPayload,
	})
	if legacyErr == nil && result.StatusCode >= 200 && result.StatusCode < 300 {
		writeDebugHTTPResult("email_otp_validate_legacy_success", result)
		return result, nil
	}

	if err != nil {
		return nil, err
	}
	if legacyErr != nil {
		return nil, legacyErr
	}
	writeDebugResponse("email_otp_validate", result.Body)
	return nil, fmt.Errorf("验证码验证失败: %d %s", result.StatusCode, limitBody(result.Body))
}

// resumeAndCaptureCallback 轮询 authorize/resume，尝试抓到 localhost callback。
// Why: consent 阶段可能发生多次 302，不能只看一次跳转结果，否则容易在中间 state 更新时丢掉最终 callback。
func (c *protocolClient) resumeAndCaptureCallback(ctx context.Context, state string) (string, error) {
	resumeURL := fmt.Sprintf(authAuthorizeResume, url.QueryEscape(state))
	for attempt := 0; attempt < 8; attempt++ {
		result, err := c.doRequest(ctx, requestOptions{
			Method:        http.MethodGet,
			URL:           resumeURL,
			AllowRedirect: false,
			Accept:        auth0AcceptHeader,
			Referer:       auth0BaseURL,
			Profile:       profileNavigate,
		})
		if err != nil {
			return "", fmt.Errorf("恢复 OAuth 会话失败: %w", err)
		}

		if strings.Contains(result.Location, "localhost:1455") {
			return result.Location, nil
		}
		if strings.Contains(result.FinalURL, "localhost:1455") {
			return result.FinalURL, nil
		}
		if result.Location != "" && extractState(result.Location) != "" {
			state = extractState(result.Location)
			resumeURL = fmt.Sprintf(authAuthorizeResume, url.QueryEscape(state))
		}

		if needsEmailOTP(result.Location) || needsEmailOTP(result.FinalURL) {
			return "", fmt.Errorf("OAuth 恢复阶段再次进入 OTP 挑战，需要先验证邮箱")
		}
	}

	return "", fmt.Errorf("未捕获到 localhost callback 跳转")
}

func needsEmailOTP(location string) bool {
	location = strings.ToLower(location)
	return strings.Contains(location, "email-otp") ||
		strings.Contains(location, "email-verification") ||
		strings.Contains(location, "mfa-challenge")
}

func extractState(raw string) string {
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err == nil {
		if state := parsed.Query().Get("state"); state != "" {
			return state
		}
	}

	matches := statePattern.FindStringSubmatch(raw)
	if len(matches) == 2 {
		value, decodeErr := url.QueryUnescape(matches[1])
		if decodeErr == nil {
			return value
		}
		return matches[1]
	}

	matches = stateValuePattern.FindStringSubmatch(raw)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

func trimForLog(state string) string {
	if len(state) <= 16 {
		return state
	}
	return state[:16] + "..."
}

func limitBody(body string) string {
	body = strings.TrimSpace(body)
	if len(body) <= 240 {
		return body
	}
	return body[:240]
}

func sanitizeFilename(value string) string {
	if value == "" {
		return "unknown"
	}

	builder := strings.Builder{}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

// doRequest 统一发起底层 HTTP 请求，并保留状态码、Location、最终 URL 与响应头。
// Why: 这条链路高度依赖 302/最终地址/响应体三者同时判断下一步，只返回 body 会丢失关键证据。
func (c *protocolClient) doRequest(ctx context.Context, options requestOptions) (*httpResult, error) {
	var body io.Reader
	if len(options.Form) > 0 {
		body = strings.NewReader(options.Form.Encode())
	} else {
		body = options.Body
	}

	req, err := http.NewRequestWithContext(ctx, options.Method, options.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header = buildBrowserHeaders(options, c.userAgent)

	c.httpClient.SetFollowRedirect(options.AllowRedirect)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	rawBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}

	return &httpResult{
		StatusCode: resp.StatusCode,
		Location:   resp.Header.Get("Location"),
		FinalURL:   resp.Request.URL.String(),
		Body:       string(rawBody),
		Header:     resp.Header.Clone(),
	}, nil
}

// buildBrowserHeaders 根据请求类型组装尽量贴近真实浏览器的头部集合。
// Why: 这里除了值本身，还会显式控制 header 顺序，目的是减少 TLS 指纹之外的浏览器画像偏差。
func buildBrowserHeaders(options requestOptions, userAgent string) http.Header {
	headers := http.Header{
		"user-agent":                  {userAgent},
		"accept-language":             {"zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7"},
		"sec-ch-ua":                   {chromeSecCHUA},
		"sec-ch-ua-full-version-list": {chromeSecCHUAFull},
		"sec-ch-ua-mobile":            {"?0"},
		"sec-ch-ua-platform":          {`"macOS"`},
		"sec-ch-ua-platform-version":  {`"15.3.1"`},
		"sec-ch-ua-arch":              {`"x86"`},
		"sec-ch-ua-bitness":           {`"64"`},
		"sec-ch-ua-model":             {`""`},
		"priority":                    {"u=0, i"},
		http.PHeaderOrderKey:          {":method", ":authority", ":scheme", ":path"},
	}
	if options.Accept != "" {
		headers["accept"] = []string{options.Accept}
	}
	if options.ContentType != "" {
		headers["content-type"] = []string{options.ContentType}
	}
	if options.Referer != "" {
		headers["referer"] = []string{options.Referer}
	}
	if options.Origin != "" {
		headers["origin"] = []string{options.Origin}
	}
	for key, value := range options.ExtraHeaders {
		headers[key] = []string{value}
	}
	applyBrowserRequestProfile(headers, options.Profile)
	return headers
}

// applyBrowserRequestProfile 为不同类型请求补齐 fetch/navigation 特征头。
// Why: 页面导航、XHR API 和 token 交换在浏览器里天然长得不一样，把它们混用会明显增加被风控识别的概率。
func applyBrowserRequestProfile(headers http.Header, profile requestProfile) {
	switch profile {
	case profileNavigate:
		headers["cache-control"] = []string{"max-age=0"}
		headers["sec-fetch-dest"] = []string{"document"}
		headers["sec-fetch-mode"] = []string{"navigate"}
		headers["sec-fetch-site"] = []string{"none"}
		headers["sec-fetch-user"] = []string{"?1"}
		headers["upgrade-insecure-requests"] = []string{"1"}
		headers["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
		headers[http.HeaderOrderKey] = []string{"cache-control", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "accept-encoding", "accept-language", "priority"}
	case profileForm:
		headers["cache-control"] = []string{"max-age=0"}
		headers["sec-fetch-dest"] = []string{"document"}
		headers["sec-fetch-mode"] = []string{"navigate"}
		headers["sec-fetch-site"] = []string{"same-origin"}
		headers["sec-fetch-user"] = []string{"?1"}
		headers["upgrade-insecure-requests"] = []string{"1"}
		headers["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
		headers[http.HeaderOrderKey] = []string{"cache-control", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "origin", "content-type", "upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "priority"}
	case profileAPI:
		headers["sec-fetch-dest"] = []string{"empty"}
		headers["sec-fetch-mode"] = []string{"cors"}
		headers["sec-fetch-site"] = []string{"same-origin"}
		headers["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
		headers[http.HeaderOrderKey] = []string{"sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "priority"}
	case profileToken:
		headers["sec-fetch-dest"] = []string{"empty"}
		headers["sec-fetch-mode"] = []string{"cors"}
		headers["sec-fetch-site"] = []string{"same-site"}
		headers["accept-encoding"] = []string{"gzip, deflate, br, zstd"}
		headers[http.HeaderOrderKey] = []string{"sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type", "sec-ch-ua-mobile", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding", "accept-language", "priority"}
	}
}
