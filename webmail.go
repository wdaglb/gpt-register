package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go-register/utils"
)

var otpPattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`)

const waitProgressInterval = 1 * time.Second

// webMailClient 封装邮箱池服务请求。
// Why: 注册、登录和后续批量任务都会依赖同一套租约/取码接口，集中封装后可以统一处理错误和 JSON 拆包。
type webMailClient struct {
	baseURL string
	client  *http.Client
	logf    func(string, ...any)
}

type webMailAccount struct {
	ID         int    `json:"id"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	LeaseToken string `json:"lease_token"`
}

type latestMailPayload struct {
	Account    webMailAccount `json:"account"`
	Mailbox    string         `json:"mailbox"`
	LatestMail map[string]any `json:"latest_mail"`
}

type webMailEnvelope[T any] struct {
	OK    bool   `json:"ok"`
	Data  T      `json:"data"`
	Error string `json:"error"`
}

// newWebMailClient 创建邮箱池 HTTP 客户端，并统一设置请求超时。
func newWebMailClient(baseURL string, timeout time.Duration) *webMailClient {
	return &webMailClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// setWaitLogger 为 web_mail 客户端设置等待日志输出。
// Why: 非 webmail 模式下很多“卡住”其实发生在本地邮箱池 HTTP 调用，客户端需要有能力把等待心跳打到主日志里。
func (c *webMailClient) setWaitLogger(logf func(string, ...any)) {
	if c == nil {
		return
	}
	c.logf = logf
}

func (c *webMailClient) leaseAccount(ctx context.Context) (*webMailAccount, error) {
	account := new(webMailAccount)
	if err := c.request(ctx, http.MethodPost, "/api/email-pool/lease", nil, nil, account); err != nil {
		return nil, err
	}
	return account, nil
}

func (c *webMailClient) returnAccount(ctx context.Context, accountID int, leaseToken string) error {
	payload := map[string]string{"lease_token": leaseToken}
	return c.request(ctx, http.MethodPost, fmt.Sprintf("/api/email-pool/accounts/%d/return", accountID), nil, payload, &struct{}{})
}

func (c *webMailClient) markUsed(ctx context.Context, accountID int, leaseToken string) error {
	payload := map[string]string{"lease_token": leaseToken}
	return c.request(ctx, http.MethodPost, fmt.Sprintf("/api/email-pool/accounts/%d/mark-used", accountID), nil, payload, &struct{}{})
}

// getStats 获取当前邮箱池统计。
// Why: TUI 底部需要展示“可用 / 已使用 / 未使用 / 租用中”，统一走 web_mail 的 stats 接口可避免重复统计逻辑。
func (c *webMailClient) getStats(ctx context.Context) (emailPoolStats, error) {
	stats := emailPoolStats{}
	if err := c.request(ctx, http.MethodGet, "/api/email-pool/stats", nil, nil, &stats); err != nil {
		return emailPoolStats{}, err
	}
	return stats, nil
}

func (c *webMailClient) getLatestMailByEmail(ctx context.Context, email, mailbox string) (*latestMailPayload, error) {
	query := url.Values{}
	query.Set("email", email)
	query.Set("mailbox", mailbox)

	result := new(latestMailPayload)
	if err := c.request(ctx, http.MethodGet, "/api/email-pool/accounts/by-email/latest", query, nil, result); err != nil {
		return nil, err
	}
	return result, nil
}

// getLatestMailByAccount 按租约账号查询最新邮件。
// Why: 注册模式是从邮箱池动态租号，按 account_id 取信可以避免并发情况下同邮箱查询串信。
func (c *webMailClient) getLatestMailByAccount(ctx context.Context, accountID int, mailbox string) (*latestMailPayload, error) {
	query := url.Values{}
	query.Set("mailbox", mailbox)

	result := new(latestMailPayload)
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/email-pool/accounts/%d/latest", accountID), query, nil, result); err != nil {
		return nil, err
	}
	return result, nil
}

// waitCodeByAccount 轮询租约账号对应的验证码邮件。
// Why: 保留 Junk -> INBOX 的回退顺序，是因为真实环境下验证码经常先落垃圾箱，再回收或同步到收件箱。
func (c *webMailClient) waitCodeByAccount(ctx context.Context, accountID int, mailbox string, pollInterval time.Duration, since time.Time, onProgress func(time.Duration)) (string, error) {
	deadline, hasDeadline := ctx.Deadline()
	mailboxes := []string{mailbox}
	if !strings.EqualFold(mailbox, "INBOX") {
		mailboxes = append(mailboxes, "INBOX")
	}

	var lastErr error
	waitStartedAt := time.Now()
	var lastReported time.Duration
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询结束，最后一次错误: %w", lastErr)
			}
			return "", err
		}

		for _, currentMailbox := range mailboxes {
			payload, err := c.getLatestMailByAccount(ctx, accountID, currentMailbox)
			if err != nil {
				lastErr = err
				continue
			}

			mailDate := extractMailDate(payload.LatestMail)
			if !since.IsZero() && !mailDate.IsZero() && mailDate.Before(since) {
				continue
			}

			if code := extractOTP(payload.LatestMail); code != "" {
				return code, nil
			}
		}

		if hasDeadline && time.Now().After(deadline) {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询超时，最后一次错误: %w", lastErr)
			}
			return "", fmt.Errorf("验证码轮询超时")
		}

		utils.MaybeReportWaitProgress(waitStartedAt, &lastReported, waitProgressInterval, onProgress)
		if err := sleepContext(ctx, pollInterval); err != nil {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询结束，最后一次错误: %w", lastErr)
			}
			return "", err
		}
	}
}

// waitCodeByEmail 兼容旧登录流程按邮箱地址轮询验证码。
// Note: 新注册链路优先使用 waitCodeByAccount，只有历史单账号登录链路仍按 email 查询。
func (c *webMailClient) waitCodeByEmail(ctx context.Context, email, mailbox string, pollInterval time.Duration, since time.Time, onProgress func(time.Duration)) (string, error) {
	deadline, hasDeadline := ctx.Deadline()
	mailboxes := []string{mailbox}
	if !strings.EqualFold(mailbox, "INBOX") {
		mailboxes = append(mailboxes, "INBOX")
	}

	var lastErr error
	waitStartedAt := time.Now()
	var lastReported time.Duration
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询结束，最后一次错误: %w", lastErr)
			}
			return "", err
		}

		for _, currentMailbox := range mailboxes {
			payload, err := c.getLatestMailByEmail(ctx, email, currentMailbox)
			if err != nil {
				lastErr = err
				continue
			}

			mailDate := extractMailDate(payload.LatestMail)
			if !since.IsZero() && !mailDate.IsZero() && mailDate.Before(since) {
				continue
			}

			if code := extractOTP(payload.LatestMail); code != "" {
				return code, nil
			}
		}

		if hasDeadline && time.Now().After(deadline) {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询超时，最后一次错误: %w", lastErr)
			}
			return "", fmt.Errorf("验证码轮询超时")
		}

		utils.MaybeReportWaitProgress(waitStartedAt, &lastReported, waitProgressInterval, onProgress)
		if err := sleepContext(ctx, pollInterval); err != nil {
			if lastErr != nil {
				return "", fmt.Errorf("验证码轮询结束，最后一次错误: %w", lastErr)
			}
			return "", err
		}
	}
}

// request 统一处理 web_mail 请求、响应 envelope 校验和错误翻译。
// Why: 服务端接口都返回同一类 JSON 信封结构，集中在这里拆包，上层业务才能专注于租约和验证码流程本身。
func (c *webMailClient) request(ctx context.Context, method, path string, query url.Values, payload any, target any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("序列化请求体失败: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	requestLabel := "web_mail请求 " + utils.FormatHTTPRequestLabel(method, fullURL)
	var resp *http.Response
	err = utils.RunWithWaitLog(ctx, utils.WaitLogFunc(c.logf, requestLabel), func() error {
		var requestErr error
		resp, requestErr = c.client.Do(req)
		return requestErr
	})
	if err != nil {
		return fmt.Errorf("调用 web_mail 失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取 web_mail 响应失败: %w", err)
	}

	envelope := webMailEnvelope[json.RawMessage]{}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return fmt.Errorf("web_mail 响应不是合法 JSON: %s", string(rawBody))
	}
	if resp.StatusCode >= http.StatusBadRequest || !envelope.OK {
		if envelope.Error != "" {
			return errors.New(envelope.Error)
		}
		return fmt.Errorf("web_mail 请求失败: HTTP %d", resp.StatusCode)
	}
	if target == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return fmt.Errorf("解析 web_mail 响应失败: %w", err)
	}
	return nil
}

// extractMailDate 提取邮件时间，用于过滤旧验证码。
func extractMailDate(latestMail map[string]any) time.Time {
	dateValue, _ := nestedString(latestMail, "data", "date")
	if dateValue == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339Nano, strings.ReplaceAll(dateValue, "Z", "+00:00"))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// extractOTP 从多个候选字段中提取 6 位验证码。
// Why: 不同来源的邮件正文字段命名并不统一，因此需要同时扫描主题、正文和整个 JSON 兜底。
func extractOTP(latestMail map[string]any) string {
	candidates := make([]string, 0, 6)
	for _, key := range []string{"subject", "text", "textBody", "html", "htmlBody"} {
		value, _ := nestedString(latestMail, "data", key)
		if value != "" {
			candidates = append(candidates, value)
		}
	}

	encoded, err := json.Marshal(latestMail)
	if err == nil {
		candidates = append(candidates, string(encoded))
	}

	for _, candidate := range candidates {
		match := otpPattern.FindStringSubmatch(candidate)
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

// nestedString 安全读取动态 JSON 的嵌套字符串字段，避免每个调用方都重复做类型断言。
func nestedString(root map[string]any, keys ...string) (string, bool) {
	current := any(root)
	for _, key := range keys {
		nextMap, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = nextMap[key]
		if !ok {
			return "", false
		}
	}

	result, ok := current.(string)
	return result, ok
}

// sleepContext 让轮询等待既能定时苏醒，也能在 ctx 取消时立即退出。
func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
