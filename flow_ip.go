package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
	"unicode"

	"go-register/utils"
)

const (
	flowUnknownIP  = "ip-unknown"
	flowIPProbeURL = "https://api.ip.cc"
)

// prepareFlowClient 在单账号流程开始时统一创建协议客户端并探测实际出口 IP。
// Why: 用户要求 IP 查询也使用真实浏览器指纹，因此这里直接复用 protocolClient，保证 IP 探测和后续主流程走同一条链路。
func prepareFlowClient(parent context.Context, cfg config, logger *log.Logger, threadLabel string) (config, *protocolClient, string, error) {
	cfg.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)

	client, err := newProtocolClient(cfg, logger)
	if err != nil {
		return cfg, nil, flowUnknownIP, err
	}

	flowIP := flowUnknownIP
	probeCtx, cancel := context.WithTimeout(parent, flowIPProbeTimeout(cfg))
	defer cancel()

	resolvedIP, err := resolveFlowPublicIPWithClient(probeCtx, client)
	if err != nil {
		if logger != nil {
			logger.Printf("[%s][%s] 获取实际IP失败: %v", strings.TrimSpace(threadLabel), flowUnknownIP, err)
		}
		return cfg, client, flowIP, nil
	}

	flowIP = resolvedIP
	if logger != nil {
		logger.Printf("[%s][%s] 实际IP=%s", strings.TrimSpace(threadLabel), flowIP, flowIP)
	}
	return cfg, client, flowIP, nil
}

// resolveFlowPublicIPWithClient 通过真实浏览器指纹客户端查询当前出口 IP。
// Why: 只有复用同一个 TLS/UA/代理链路，日志里的实际 IP 才能真正对应当前账号流程。
func resolveFlowPublicIPWithClient(ctx context.Context, client *protocolClient) (string, error) {
	if client == nil {
		return "", fmt.Errorf("nil protocol client")
	}

	result, err := client.doRequest(ctx, requestOptions{
		Method:        "GET",
		URL:           flowIPProbeURL,
		AllowRedirect: true,
		Accept:        "application/json,text/plain;q=0.9,*/*;q=0.8",
		Profile:       profileAPI,
	})
	if err != nil {
		return "", fmt.Errorf("请求 %s 失败: %w", flowIPProbeURL, err)
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return "", fmt.Errorf("请求 %s 返回异常状态: %d body=%s", flowIPProbeURL, result.StatusCode, limitBody(result.Body))
	}

	ip := extractFirstIP(result.Body)
	if ip == "" {
		return "", fmt.Errorf("%s 未返回合法IP body=%s", flowIPProbeURL, limitBody(result.Body))
	}
	return ip, nil
}

// extractFirstIP 优先从 JSON 字段中递归提取 IP，再回退到文本扫描。
// Why: api.ip.cc 返回 JSON；递归取值可以兼容不同字段名，避免把整个 JSON 串打印进日志前缀。
func extractFirstIP(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	var payload any
	if err := json.Unmarshal([]byte(body), &payload); err == nil {
		if ip := extractFirstIPFromValue(payload); ip != "" {
			return ip
		}
	}

	return extractFirstIPFromText(body)
}

func extractFirstIPFromValue(value any) string {
	switch typed := value.(type) {
	case string:
		return extractFirstIPFromText(typed)
	case []any:
		for _, item := range typed {
			if ip := extractFirstIPFromValue(item); ip != "" {
				return ip
			}
		}
	case map[string]any:
		for _, item := range typed {
			if ip := extractFirstIPFromValue(item); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func extractFirstIPFromText(text string) string {
	tokens := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`"'<>[](){},`, r)
	})
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		token = strings.Trim(token, ":;")
		if ip := net.ParseIP(token); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// buildFlowLogPrefix 构造带线程与出口 IP 的统一日志前缀。
// Why: 账号流程日志需要稳定包含线程和 IP，后续再拼接邮箱时也应复用同一前缀格式，避免不同阶段前缀漂移。
func buildFlowLogPrefix(threadLabel, flowIP, email string) string {
	parts := []string{"[" + strings.TrimSpace(threadLabel) + "]"}
	if strings.TrimSpace(flowIP) == "" {
		parts = append(parts, "["+flowUnknownIP+"]")
	} else {
		parts = append(parts, "["+strings.TrimSpace(flowIP)+"]")
	}
	if strings.TrimSpace(email) != "" {
		parts = append(parts, "["+strings.TrimSpace(email)+"]")
	}
	return strings.Join(parts, "")
}

// flowIPProbeTimeout 为出口 IP 探测单独提供较短超时，避免探测失败拖慢主流程。
// Why: 实际 IP 只是排障辅助信息，不应和主业务请求共享完整超时。
func flowIPProbeTimeout(cfg config) time.Duration {
	if cfg.requestTimeout > 0 && cfg.requestTimeout < 5*time.Second {
		return cfg.requestTimeout
	}
	return 5 * time.Second
}

// newFlowScopedLogger 为单账号流程创建带固定前缀的子 logger。
// Why: 用户要求把线程和实际 IP 挂到整条流程日志上，创建子 logger 比每条日志手工拼前缀更稳定。
func newFlowScopedLogger(base *log.Logger, prefix string) *log.Logger {
	if base == nil {
		return nil
	}
	return log.New(base.Writer(), base.Prefix()+strings.TrimSpace(prefix)+" ", base.Flags())
}
