package main

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// runWithWaitLog 在同步操作执行期间按固定间隔输出等待心跳。
// Why: 不论是 HTTP 请求、清理动作还是其他外部依赖调用，只要超过 1 秒没有返回，
// 都应该告诉用户“当前还在等哪一步”，避免线程看起来像彻底卡死。
func runWithWaitLog(ctx context.Context, onProgress func(time.Duration), fn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	waitTicker := time.NewTicker(time.Second)
	defer waitTicker.Stop()

	waited := time.Duration(0)
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-waitTicker.C:
			waited += time.Second
			if onProgress != nil {
				onProgress(waited)
			}
		}
	}
}

// waitLogFunc 基于 logger 和动作标签生成统一的等待日志回调。
// Why: 各层日志文案需要保持一致，统一封装后可以避免“等待中”“卡住了”等文案四处漂移。
func waitLogFunc(logf func(string, ...any), label string) func(time.Duration) {
	if logf == nil || strings.TrimSpace(label) == "" {
		return nil
	}
	return func(elapsed time.Duration) {
		logf("等待%s %s...", label, elapsed)
	}
}

// formatHTTPRequestLabel 把 HTTP 方法和 URL 压缩成适合日志展示的短标签。
// Why: 直接打印完整 URL 容易把日志刷得很长，只保留 host/path 更利于快速定位堵在了哪类请求。
func formatHTTPRequestLabel(method, rawURL string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimSpace(method + " " + rawURL)
	}

	target := parsed.Host
	if parsed.Path != "" {
		target += parsed.Path
	}
	if target == "" {
		target = rawURL
	}
	if method == "" {
		return target
	}
	return method + " " + target
}
