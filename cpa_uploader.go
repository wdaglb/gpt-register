package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cpaUploadResponse 对齐 CPA 管理接口的通用响应结构。
// Why: 上传接口失败时通常会在 error 字段里返回更具体的原因，解出来后日志更容易排查。
type cpaUploadResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// maybeUploadAuthFileToCPA 在配置了 CPA 地址时，把本地 auth JSON 自动同步到远端。
// Why: 当前 Go 版本已经能在本地生成可直接使用的 auth 文件，因此这里直接上传产物，
// 不再复刻 Python 脚本里“重新向 CPA 申请授权链接再转发 callback”的整条外部链路。
func maybeUploadAuthFileToCPA(ctx context.Context, cfg config, email, authFilePath string) (bool, error) {
	if strings.TrimSpace(cfg.cpaURL) == "" {
		return false, nil
	}
	if strings.TrimSpace(cfg.cpaKey) == "" {
		return false, fmt.Errorf("已配置 cpa-url 但缺少 cpa-key")
	}
	if strings.TrimSpace(authFilePath) == "" {
		return false, fmt.Errorf("账号 %s 的 auth 文件路径为空", strings.TrimSpace(email))
	}

	body, err := os.ReadFile(authFilePath)
	if err != nil {
		return true, fmt.Errorf("读取 auth 文件失败: %w", err)
	}

	uploadURL := fmt.Sprintf("%s/v0/management/auth-files?name=%s", cfg.cpaURL, url.QueryEscape(filepath.Base(authFilePath)))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body))
	if err != nil {
		return true, fmt.Errorf("创建 CPA 上传请求失败: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.cpaKey)
	request.Header.Set("Content-Type", "application/json")

	timeout := cfg.requestTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return true, fmt.Errorf("请求 CPA 上传接口失败: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	rawBody, err := io.ReadAll(response.Body)
	if err != nil {
		return true, fmt.Errorf("读取 CPA 上传响应失败: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return true, fmt.Errorf("CPA 上传返回异常状态: %d body=%s", response.StatusCode, trimHTTPBodyForLog(rawBody))
	}

	parsed := cpaUploadResponse{}
	if len(rawBody) > 0 && json.Unmarshal(rawBody, &parsed) == nil && !parsed.OK {
		return true, fmt.Errorf("CPA 上传失败: %s", strings.TrimSpace(parsed.Error))
	}
	return true, nil
}

// trimHTTPBodyForLog 控制错误日志里的响应体长度，避免异常页把终端刷满。
func trimHTTPBodyForLog(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) <= 240 {
		return trimmed
	}
	return trimmed[:240] + "..."
}
