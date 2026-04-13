package main

import (
	"context"
	"fmt"
	"io"
	"log"
	stdhttp "net/http"
	"net/url"
	"strings"
	"time"

	"go-register/utils"
)

const flowUnknownIP = "ip-unknown"

type flowLogger interface {
	Printf(string, ...any)
}

// resolveFlowPublicIP 通过当前流程代理链路查询真实出口 IP。
// Why: 用户需要按“单账号流程”观察真实出口 IP，因此这里显式复用当前流程代理，避免日志中的 IP 与实际请求链路不一致。
func resolveFlowPublicIP(ctx context.Context, cfg config) (string, error) {
	transport := &stdhttp.Transport{}
	if strings.TrimSpace(cfg.proxy) != "" {
		proxyURL, err := url.Parse(strings.TrimSpace(cfg.proxy))
		if err != nil {
			return "", fmt.Errorf("解析 IP 探测代理失败: %w", err)
		}
		transport.Proxy = stdhttp.ProxyURL(proxyURL)
	}

	client := &stdhttp.Client{
		Timeout:   cfg.requestTimeout,
		Transport: transport,
	}
	request, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://ping0.cc", nil)
	if err != nil {
		return "", fmt.Errorf("创建 IP 探测请求失败: %w", err)
	}
	request.Header.Set("User-Agent", defaultOAuthUserAgent)

	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("请求 ping0.cc 失败: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil {
		return "", fmt.Errorf("读取 ping0.cc 响应失败: %w", err)
	}
	ip := strings.TrimSpace(string(body))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("请求 ping0.cc 返回异常状态: %d body=%s", response.StatusCode, ip)
	}
	if ip == "" {
		return "", fmt.Errorf("ping0.cc 返回空 IP")
	}
	return ip, nil
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

// prepareFlowLogging 在账号流程启动时解析代理占位符并记录当前出口 IP。
// Why: register/login/authorize 都需要在真正访问 OpenAI 前先拿到本次流程的固定 IP，便于后续整条链路排障。
func prepareFlowLogging(parent context.Context, cfg config, logger flowLogger, threadLabel string) (config, string) {
	cfg.proxy = utils.ResolveProxyPlaceholders(cfg.proxy)
	flowIP := flowUnknownIP

	probeCtx, cancel := context.WithTimeout(parent, flowIPProbeTimeout(cfg))
	defer cancel()

	resolvedIP, err := resolveFlowPublicIP(probeCtx, cfg)
	if err != nil {
		if logger != nil {
			logger.Printf("[%s][%s] 获取实际IP失败: %v", strings.TrimSpace(threadLabel), flowUnknownIP, err)
		}
		return cfg, flowIP
	}

	flowIP = resolvedIP
	if logger != nil {
		logger.Printf("[%s][%s] 实际IP=%s", strings.TrimSpace(threadLabel), flowIP, flowIP)
	}
	return cfg, flowIP
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
