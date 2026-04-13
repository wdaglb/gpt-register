package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const accountFieldDelimiter = "----"

// accountRecord 表示 accounts.txt 中的一条账号状态记录。
// Why: 这里把“注册状态”和“授权状态”放在同一行，是为了让注册线程和授权线程通过一个共享状态源协作，而不是各写各的日志文件。
type accountRecord struct {
	Email          string
	Password       string
	RegisterStatus string
	RegisterTime   string
	OAuthStatus    string
	OAuthTime      string
	AuthFilePath   string
}

// accountsStore 负责线程安全地读写 accounts.txt。
// Why: 注册线程和授权线程都可能同时更新同一文件，这里同时使用进程内互斥锁和文件锁，避免出现内容互相覆盖或半行写入。
type accountsStore struct {
	path       string
	mu         sync.Mutex
	waitLogger func(string, time.Duration)
}

func newAccountsStore(path string) *accountsStore {
	return &accountsStore{path: filepath.Clean(path)}
}

// setWaitLogger 为 accounts 文件锁等待设置日志输出。
// Why: 运行期 logger 只有在 executeMode/runWithTUI 内才确定，因此 store 需要允许后注入日志器。
func (s *accountsStore) setWaitLogger(logger *log.Logger) {
	if s == nil {
		return
	}
	if logger == nil {
		s.waitLogger = nil
		return
	}
	s.waitLogger = func(resource string, elapsed time.Duration) {
		logger.Printf("等待%s %s...", resource, elapsed.Truncate(time.Second))
	}
}

// upsertRegistration 写入或更新账号的注册结果。
func (s *accountsStore) upsertRegistration(email, password, registerStatus string, registerTime time.Time) (accountRecord, error) {
	record := accountRecord{
		Email:          strings.TrimSpace(email),
		Password:       strings.TrimSpace(password),
		RegisterStatus: strings.TrimSpace(registerStatus),
		RegisterTime:   formatAccountTimestamp(registerTime),
		OAuthStatus:    "oauth=pending",
	}
	if record.RegisterStatus != "ok" {
		record.OAuthStatus = ""
	}

	var updated accountRecord
	err := s.mutate(func(records []accountRecord) ([]accountRecord, error) {
		index := findAccountRecordIndex(records, record.Email)
		if index >= 0 {
			existing := records[index]
			existing.Password = record.Password
			existing.RegisterStatus = record.RegisterStatus
			existing.RegisterTime = record.RegisterTime
			if record.RegisterStatus != "ok" {
				existing.OAuthStatus = ""
				existing.OAuthTime = ""
				existing.AuthFilePath = ""
			} else if normalizeOAuthStatus(existing.OAuthStatus) == "" {
				existing.OAuthStatus = record.OAuthStatus
			}
			records[index] = existing
			updated = existing
			return records, nil
		}

		records = append(records, record)
		updated = record
		return records, nil
	})
	if err != nil {
		return accountRecord{}, err
	}
	return updated, nil
}

// upsertOAuthResult 写入或更新账号的授权结果。
func (s *accountsStore) upsertOAuthResult(email, password, oauthStatus string, oauthTime time.Time, authFilePath string) (accountRecord, error) {
	normalizedStatus := normalizeOAuthStatus(oauthStatus)
	record := accountRecord{
		Email:        strings.TrimSpace(email),
		Password:     strings.TrimSpace(password),
		OAuthStatus:  normalizedStatus,
		OAuthTime:    formatAccountTimestamp(oauthTime),
		AuthFilePath: strings.TrimSpace(authFilePath),
	}

	var updated accountRecord
	err := s.mutate(func(records []accountRecord) ([]accountRecord, error) {
		index := findAccountRecordIndex(records, record.Email)
		if index >= 0 {
			existing := records[index]
			if record.Password != "" {
				existing.Password = record.Password
			}
			existing.OAuthStatus = record.OAuthStatus
			existing.OAuthTime = record.OAuthTime
			existing.AuthFilePath = record.AuthFilePath
			records[index] = existing
			updated = existing
			return records, nil
		}

		records = append(records, record)
		updated = record
		return records, nil
	})
	if err != nil {
		return accountRecord{}, err
	}
	return updated, nil
}

// listPendingAuthorization 返回已注册成功但尚未授权成功的账号。
func (s *accountsStore) listPendingAuthorization() ([]accountRecord, error) {
	records, err := s.readAll()
	if err != nil {
		return nil, err
	}

	pending := make([]accountRecord, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.Email) == "" || strings.TrimSpace(record.Password) == "" {
			continue
		}
		if !isRegisterSuccessful(record.RegisterStatus) {
			continue
		}
		if isOAuthSuccessful(record.OAuthStatus) {
			continue
		}
		pending = append(pending, record)
	}
	return pending, nil
}

func (s *accountsStore) readAll() ([]accountRecord, error) {
	var records []accountRecord
	err := s.withLockedFile(func() error {
		loaded, err := s.loadRecordsUnlocked()
		if err != nil {
			return err
		}
		records = loaded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (s *accountsStore) mutate(mutator func([]accountRecord) ([]accountRecord, error)) error {
	return s.withLockedFile(func() error {
		records, err := s.loadRecordsUnlocked()
		if err != nil {
			return err
		}

		updated, err := mutator(records)
		if err != nil {
			return err
		}
		return s.saveRecordsUnlocked(updated)
	})
}

func (s *accountsStore) withLockedFile(fn func() error) error {
	lockMutexWithProgress(&s.mu, func(elapsed time.Duration) {
		s.logWaitProgress("accounts 进程内锁", elapsed)
	})
	defer s.mu.Unlock()

	lockPath := s.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("创建 accounts 锁目录失败: %w", err)
	}

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("打开 accounts 锁文件失败: %w", err)
	}
	defer func() {
		_ = lockFile.Close()
	}()

	if err := lockFileExclusiveWithProgress(lockFile, func(elapsed time.Duration) {
		s.logWaitProgress("accounts 文件锁", elapsed)
	}); err != nil {
		return fmt.Errorf("锁定 accounts 文件失败: %w", err)
	}
	defer func() {
		_ = unlockFile(lockFile)
	}()

	return fn()
}

func (s *accountsStore) logWaitProgress(resource string, elapsed time.Duration) {
	if s == nil || s.waitLogger == nil {
		return
	}
	s.waitLogger(resource, elapsed)
}

func (s *accountsStore) loadRecordsUnlocked() ([]accountRecord, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []accountRecord{}, nil
		}
		return nil, fmt.Errorf("读取 accounts 文件失败: %w", err)
	}

	records := make([]accountRecord, 0)
	for _, line := range strings.Split(string(raw), "\n") {
		record, ok := parseAccountLine(line)
		if ok {
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *accountsStore) saveRecordsUnlocked(records []accountRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建 accounts 目录失败: %w", err)
	}

	builder := strings.Builder{}
	for _, record := range records {
		builder.WriteString(serializeAccountRecord(record))
		builder.WriteByte('\n')
	}

	tempFile, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("创建 accounts 临时文件失败: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := tempFile.WriteString(builder.String()); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("写入 accounts 临时文件失败: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("关闭 accounts 临时文件失败: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("替换 accounts 文件失败: %w", err)
	}
	return nil
}

func parseAccountLine(line string) (accountRecord, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.Contains(line, accountFieldDelimiter) {
		return accountRecord{}, false
	}

	parts := strings.Split(line, accountFieldDelimiter)
	if len(parts) < 2 {
		return accountRecord{}, false
	}

	record := accountRecord{
		Email:    strings.TrimSpace(parts[0]),
		Password: strings.TrimSpace(parts[1]),
	}

	switch {
	case len(parts) >= 4 && looksLikeAccountTimestamp(parts[3]):
		record.RegisterStatus = strings.TrimSpace(parts[2])
		record.RegisterTime = strings.TrimSpace(parts[3])
		if len(parts) >= 5 {
			record.OAuthStatus = normalizeOAuthStatus(parts[4])
		}
		if len(parts) >= 6 {
			record.OAuthTime = strings.TrimSpace(parts[5])
		}
		if len(parts) >= 7 {
			record.AuthFilePath = strings.TrimSpace(parts[6])
		}
	case len(parts) >= 4:
		record.OAuthStatus = normalizeOAuthStatus(parts[2] + ":" + parts[3])
	default:
		record.OAuthStatus = normalizeOAuthStatus(parts[2])
	}

	return record, record.Email != ""
}

func serializeAccountRecord(record accountRecord) string {
	fields := []string{
		strings.TrimSpace(record.Email),
		strings.TrimSpace(record.Password),
		strings.TrimSpace(record.RegisterStatus),
		strings.TrimSpace(record.RegisterTime),
		normalizeOAuthStatus(record.OAuthStatus),
		strings.TrimSpace(record.OAuthTime),
		strings.TrimSpace(record.AuthFilePath),
	}
	return strings.Join(fields, accountFieldDelimiter)
}

func normalizeOAuthStatus(status string) string {
	status = strings.TrimSpace(status)
	switch {
	case status == "":
		return ""
	case strings.HasPrefix(status, "oauth="):
		return status
	case status == "ok":
		return "oauth=ok"
	case status == "pending":
		return "oauth=pending"
	case strings.HasPrefix(status, "fail"):
		return "oauth=" + status
	default:
		return status
	}
}

func findAccountRecordIndex(records []accountRecord, email string) int {
	target := strings.TrimSpace(strings.ToLower(email))
	for index, record := range records {
		if strings.TrimSpace(strings.ToLower(record.Email)) == target {
			return index
		}
	}
	return -1
}

func looksLikeAccountTimestamp(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

func formatAccountTimestamp(timestamp time.Time) string {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return timestamp.Format("2006-01-02 15:04:05")
}

func isRegisterSuccessful(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "ok")
}

func isOAuthSuccessful(status string) bool {
	return strings.EqualFold(normalizeOAuthStatus(status), "oauth=ok")
}
