package utils

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var proxySessionPlaceholderPattern = regexp.MustCompile(`\{(\d*)\}`)

// ResolveProxyPlaceholders 把代理字符串里的 {} / {N} 占位符替换成随机会话 key。
// Why: 同一流程内代理会话 key 需要稳定，但不同流程应重新生成，因此这里按单次调用生成并复用。
func ResolveProxyPlaceholders(proxy string) string {
	if !strings.Contains(proxy, "{") {
		return proxy
	}

	sessionKeys := map[int]string{}
	return proxySessionPlaceholderPattern.ReplaceAllStringFunc(proxy, func(placeholder string) string {
		matches := proxySessionPlaceholderPattern.FindStringSubmatch(placeholder)
		if len(matches) != 2 {
			return placeholder
		}

		length := 12
		if matches[1] != "" {
			parsed, err := strconv.Atoi(matches[1])
			if err == nil && parsed > 0 {
				length = parsed
			}
		}

		sessionKey, ok := sessionKeys[length]
		if !ok {
			sessionKey = RandomAlphaNumeric(length)
			sessionKeys[length] = sessionKey
		}
		return sessionKey
	})
}

// RandomAlphaNumeric 生成指定长度的字母数字随机串。
func RandomAlphaNumeric(length int) string {
	return RandomCharset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", length)
}

// RandomCharset 从给定字符集里均匀抽样，避免引入 math/rand 的可预测序列。
func RandomCharset(charset string, length int) string {
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

// RandomInt 返回闭区间 [min, max] 的随机整数。
func RandomInt(min, max int) int {
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

// RandomUUID 生成 UUIDv4 字符串。
func RandomUUID() string {
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

// RunWithWaitLog 在同步操作执行期间按固定间隔输出等待心跳。
func RunWithWaitLog(ctx context.Context, onProgress func(time.Duration), fn func() error) error {
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

// WaitLogFunc 基于 logger 和动作标签生成统一的等待日志回调。
func WaitLogFunc(logf func(string, ...any), label string) func(time.Duration) {
	if logf == nil || strings.TrimSpace(label) == "" {
		return nil
	}
	return func(elapsed time.Duration) {
		logf("等待%s %s...", label, elapsed)
	}
}

// FormatHTTPRequestLabel 把 HTTP 方法和 URL 压缩成适合日志展示的短标签。
func FormatHTTPRequestLabel(method, rawURL string) string {
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

// MaybeReportWaitProgress 在长轮询场景下按固定间隔输出“仍在等待”的心跳。
func MaybeReportWaitProgress(startedAt time.Time, lastReported *time.Duration, interval time.Duration, onProgress func(time.Duration)) {
	if onProgress == nil || interval <= 0 || startedAt.IsZero() || lastReported == nil {
		return
	}

	elapsed := time.Since(startedAt)
	reported := (elapsed / interval) * interval
	if reported < interval || reported <= *lastReported {
		return
	}

	*lastReported = reported
	onProgress(reported)
}
