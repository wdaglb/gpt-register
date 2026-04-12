package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMailAPIBaseURL             = "https://www.appleemail.top"
	defaultWebMailLeaseTimeoutSeconds = 600
	emailPoolStatusAvailable          = "available"
	emailPoolStatusLeased             = "leased"
	emailPoolStatusUsed               = "used"
	emailPoolReclaimCheckInterval     = 3 * time.Second
	emailPoolFieldDelimiter           = "----"
	emailPoolMetadataLeaseToken       = "lease_token"
	emailPoolMetadataLeasedAt         = "leased_at"
	emailPoolMetadataUsedAt           = "used_at"
	emailPoolMetadataStatus           = "status"
	emailPoolLegacyMetadataID         = "id"
	emailPoolLegacyMetadataCreatedAt  = "created_at"
	emailPoolLegacyMetadataUpdatedAt  = "updated_at"
	emailPoolLegacyMetadataSourceTime = "source_updated_at"
)

var emailPoolValidMailboxes = map[string]struct{}{
	"INBOX": {},
	"Junk":  {},
}

var emailPoolManagedMetadataKeys = map[string]struct{}{
	emailPoolMetadataLeaseToken:       {},
	emailPoolMetadataLeasedAt:         {},
	emailPoolMetadataUsedAt:           {},
	emailPoolMetadataStatus:           {},
	emailPoolLegacyMetadataID:         {},
	emailPoolLegacyMetadataCreatedAt:  {},
	emailPoolLegacyMetadataUpdatedAt:  {},
	emailPoolLegacyMetadataSourceTime: {},
}

// emailPoolLine 表示 emails.txt 中解析出的单行邮箱记录。
// Why: 文本文件既承载基础账号信息，也承载运行期状态后缀，因此这里把“字段值”和“字段是否显式出现”同时记录下来，后续才能正确做状态继承。
type emailPoolLine struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
	ExtraFields  []string

	Status    string
	HasStatus bool

	LeaseToken    *string
	HasLeaseToken bool

	LeasedAt    *string
	HasLeasedAt bool

	UsedAt    *string
	HasUsedAt bool
}

// emailPoolRecord 表示内存里的单条邮箱记录。
// Why: HTTP 层既要返回外部需要的账号字段，也要维护租约状态与持久化元数据，因此统一收口成一个稳定结构更容易保证读写一致。
type emailPoolRecord struct {
	ID              int
	Email           string
	Password        string
	ClientID        string
	RefreshToken    string
	Status          string
	LeaseToken      *string
	LeasedAt        *string
	UsedAt          *string
	SourceUpdatedAt string
	CreatedAt       string
	UpdatedAt       string
	ExtraFields     []string
}

// emailPoolPublicAccount 是对外暴露的邮箱池账号结构。
// Why: 继续沿用历史响应字段形状，避免现有客户端和脚本因为服务端改成 txt 存储后发生接口不兼容。
type emailPoolPublicAccount struct {
	ID              int      `json:"id"`
	Email           string   `json:"email"`
	Password        string   `json:"password"`
	ClientID        string   `json:"client_id"`
	RefreshToken    string   `json:"refresh_token"`
	Status          string   `json:"status"`
	LeaseToken      *string  `json:"lease_token"`
	LeasedAt        *string  `json:"leased_at"`
	UsedAt          *string  `json:"used_at"`
	SourceUpdatedAt string   `json:"source_updated_at"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	ExtraFields     []string `json:"extra_fields"`
}

// emailPoolStats 汇总账号池状态统计。
type emailPoolStats struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Leased    int `json:"leased"`
	Used      int `json:"used"`
}

// emailPoolSyncResult 记录一次文件同步的结果。
type emailPoolSyncResult struct {
	File     string `json:"file"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	SyncedAt string `json:"synced_at"`
	Reason   string `json:"reason,omitempty"`
}

// emailPoolReclaimResult 记录一次租约回收的结果。
type emailPoolReclaimResult struct {
	ReclaimedCount      int    `json:"reclaimed_count"`
	ReclaimedIDs        []int  `json:"reclaimed_ids"`
	CheckedAt           string `json:"checked_at"`
	LeaseTimeoutSeconds int    `json:"lease_timeout_seconds"`
}

type emailPoolSyncEnvelopeData struct {
	Sync    emailPoolSyncResult    `json:"sync"`
	Reclaim emailPoolReclaimResult `json:"reclaim"`
}

type emailPoolEnvelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

type emailPoolFileSignature struct {
	ModTimeNS int64
	Size      int64
}

// emailPoolStore 负责把 emails.txt 当作唯一数据库进行读写。
// Why: 改成 txt 持久化后，正确性取决于“进程内互斥 + 文件锁 + 原子替换写入”三层同时生效，否则多请求并发时很容易把状态写乱。
type emailPoolStore struct {
	path      string
	mu        sync.Mutex
	records   []emailPoolRecord
	signature *emailPoolFileSignature
}

// upstreamMailClient 负责请求上游邮件接口拉取最新邮件。
// Why: 账号池服务只关心“如何根据 refresh_token/client_id/email 取信”，具体 HTTP 细节应该独立封装。
type upstreamMailClient struct {
	baseURL string
	client  *http.Client
}

// emailPoolService 托管自动同步、租约回收与 HTTP 路由分发。
// Why: 文件同步和租约回收都带有缓存/节流语义，和纯存储层不是一个关注点，独立成服务层更清晰。
type emailPoolService struct {
	store               *emailPoolStore
	emailsFile          string
	mailClient          *upstreamMailClient
	leaseTimeoutSeconds int
	logger              *log.Logger
	syncMu              sync.Mutex
	fileSignature       *emailPoolFileSignature
	lastSyncResult      emailPoolSyncResult
	reclaimMu           sync.Mutex
	lastReclaimResult   emailPoolReclaimResult
	lastReclaimCheckAt  time.Time
}

// runWebMailServer 启动当前仓库内置的 web_mail HTTP 服务。
// Why: 现在直接把 emails.txt 当数据库，因此启动时先完成一次同步，确保服务能接受只有基础四列的旧文件格式。
func runWebMailServer(parent context.Context, cfg config, logger *log.Logger) error {
	if cfg.webMailDBPath != "" {
		logger.Printf("web_mail 已忽略历史参数 web-mail-db=%s，当前直接使用 %s 作为 txt 数据库", cfg.webMailDBPath, cfg.webMailEmailsFile)
	}

	store, err := newEmailPoolStore(cfg.webMailEmailsFile)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	service := newEmailPoolService(
		store,
		cfg.webMailEmailsFile,
		newUpstreamMailClient(cfg.mailAPIBase, cfg.requestTimeout),
		cfg.webMailLeaseTimeoutSeconds,
		logger,
	)

	syncResult, err := service.ensureSynced(context.Background(), true)
	if err != nil {
		return err
	}
	logger.Printf("web_mail 初始同步完成: inserted=%d updated=%d skipped=%d file=%s", syncResult.Inserted, syncResult.Updated, syncResult.Skipped, syncResult.File)

	if cfg.webMailSyncOnly {
		logger.Printf("web_mail 已按 sync-only 模式执行完毕")
		return nil
	}

	address := net.JoinHostPort(cfg.webMailHost, strconv.Itoa(cfg.webMailPort))
	server := &http.Server{
		Addr:    address,
		Handler: service,
	}

	go func() {
		<-parent.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("web_mail 服务已启动: http://%s", address)
	logger.Printf("web_mail 可用接口: /health, /api/email-pool/stats, /api/email-pool/lease, /api/email-pool/sync")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("启动 web_mail 服务失败: %w", err)
	}
	return nil
}

// parseEmailPoolLine 解析 emails.txt 中的单行记录。
// Why: 允许在基础四列后追加运行态后缀字段，既兼容历史纯账号文件，也支持当前 txt 数据库存储租约状态。
func parseEmailPoolLine(line string) (*emailPoolLine, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}

	parts := strings.Split(trimmed, emailPoolFieldDelimiter)
	if len(parts) < 4 {
		return nil, fmt.Errorf("邮箱数据字段不足 4 列: %s", trimmed)
	}

	record := &emailPoolLine{
		Email:        strings.TrimSpace(parts[0]),
		Password:     strings.TrimSpace(parts[1]),
		ClientID:     strings.TrimSpace(parts[2]),
		RefreshToken: strings.TrimSpace(parts[3]),
	}

	for _, rawField := range parts[4:] {
		field := strings.TrimSpace(rawField)
		if field == "" {
			continue
		}

		key, value, ok := parseEmailPoolMetadataField(field)
		if !ok {
			record.ExtraFields = append(record.ExtraFields, field)
			continue
		}

		switch key {
		case emailPoolMetadataLeaseToken:
			record.LeaseToken = copyStringPointerFromValue(value)
			record.HasLeaseToken = true
		case emailPoolMetadataLeasedAt:
			record.LeasedAt = copyStringPointerFromValue(value)
			record.HasLeasedAt = true
		case emailPoolMetadataUsedAt:
			record.UsedAt = copyStringPointerFromValue(value)
			record.HasUsedAt = true
		case emailPoolMetadataStatus:
			normalizedStatus, err := normalizeEmailPoolStatus(value)
			if err != nil {
				return nil, err
			}
			record.Status = normalizedStatus
			record.HasStatus = true
		case emailPoolLegacyMetadataID, emailPoolLegacyMetadataCreatedAt, emailPoolLegacyMetadataUpdatedAt, emailPoolLegacyMetadataSourceTime:
			// Why: 兼容旧版 txt/SQLite 迁移残留字段，但新格式不再把这些内部字段写回文件。
			continue
		}
	}

	return record, nil
}

func parseEmailPoolMetadataField(field string) (string, string, bool) {
	index := strings.Index(field, ":")
	if index <= 0 {
		return "", "", false
	}

	key := strings.TrimSpace(field[:index])
	if _, ok := emailPoolManagedMetadataKeys[key]; !ok {
		return "", "", false
	}
	return key, strings.TrimSpace(field[index+1:]), true
}

func normalizeEmailPoolStatus(status string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(status))
	switch normalized {
	case emailPoolStatusAvailable, emailPoolStatusLeased, emailPoolStatusUsed:
		return normalized, nil
	default:
		return "", fmt.Errorf("邮箱状态仅支持 available/leased/used，当前值=%q", status)
	}
}

// newEmailPoolStore 初始化 txt 版邮箱池存储。
// Why: 统一在这里收口目录预创建和文件路径规范化，避免调用方到处重复处理路径细节。
func newEmailPoolStore(path string) (*emailPoolStore, error) {
	cleanPath := filepath.Clean(path)
	directory := filepath.Dir(cleanPath)
	if directory != "." && directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("创建 web_mail 数据目录失败: %w", err)
		}
	}

	return &emailPoolStore{
		path: cleanPath,
	}, nil
}

// Close 为了兼容原调用点保留，txt 存储当前无需释放额外资源。
func (store *emailPoolStore) Close() error {
	return nil
}

func (store *emailPoolStore) withLockedFile(callback func() error) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	lockPath := store.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("创建 web_mail 锁目录失败: %w", err)
	}

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("打开 web_mail 锁文件失败: %w", err)
	}
	defer func() {
		_ = lockFile.Close()
	}()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("锁定 web_mail 文件失败: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	return callback()
}

// syncFromFile 把 emails.txt 同步到内存快照。
// Why: 虽然 txt 已经是唯一数据库，但服务仍然要支持外部脚本手工编辑文件，因此请求前需要把外部改动安全吸收到内存状态中。
func (store *emailPoolStore) syncFromFile(ctx context.Context, emailsFile string) (emailPoolSyncResult, error) {
	_ = ctx

	cleanPath := filepath.Clean(emailsFile)
	if cleanPath != store.path {
		return emailPoolSyncResult{}, fmt.Errorf("邮箱文件路径不匹配: want=%s got=%s", store.path, cleanPath)
	}

	result := emailPoolSyncResult{
		File:     cleanPath,
		SyncedAt: utcNowString(),
	}

	err := store.withLockedFile(func() error {
		loaded, changedByNormalization, err := store.loadRecordsUnlocked(result.SyncedAt)
		if err != nil {
			return err
		}

		result.Inserted, result.Updated, result.Skipped = diffEmailPoolRecords(store.records, loaded)
		store.records = loaded

		signature, err := getEmailPoolFileSignature(store.path)
		if err != nil {
			return fmt.Errorf("读取邮箱源文件签名失败: %w", err)
		}
		store.signature = signature

		if changedByNormalization {
			if err := store.saveRecordsUnlocked(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return emailPoolSyncResult{}, err
	}

	return result, nil
}

func (store *emailPoolStore) loadRecordsUnlocked(now string) ([]emailPoolRecord, bool, error) {
	raw, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, fmt.Errorf("读取邮箱源文件失败: %w", err)
		}
		return nil, false, fmt.Errorf("读取邮箱源文件失败: %w", err)
	}

	previousByEmail := make(map[string]emailPoolRecord, len(store.records))
	for _, record := range store.records {
		previousByEmail[normalizeEmailPoolKey(record.Email)] = cloneEmailPoolRecord(record)
	}

	records := make([]emailPoolRecord, 0)
	seenEmails := make(map[string]struct{})
	usedIDs := make(map[int]string)
	changedByNormalization := false

	for _, rawLine := range strings.Split(string(raw), "\n") {
		parsedLine, err := parseEmailPoolLine(rawLine)
		if err != nil {
			return nil, false, err
		}
		if parsedLine == nil {
			continue
		}

		emailKey := normalizeEmailPoolKey(parsedLine.Email)
		if _, exists := seenEmails[emailKey]; exists {
			return nil, false, fmt.Errorf("邮箱文件中存在重复账号: %s", parsedLine.Email)
		}
		seenEmails[emailKey] = struct{}{}

		var previous *emailPoolRecord
		if existing, ok := previousByEmail[emailKey]; ok {
			copied := existing
			previous = &copied
		}

		record, normalized, err := buildEmailPoolRecord(*parsedLine, previous, now)
		if err != nil {
			return nil, false, err
		}
		if conflictEmail, exists := usedIDs[record.ID]; exists && conflictEmail != emailKey {
			return nil, false, fmt.Errorf("邮箱账号 ID 冲突: %s 与 %s", parsedLine.Email, conflictEmail)
		}
		usedIDs[record.ID] = emailKey
		if normalized {
			changedByNormalization = true
		}
		records = append(records, record)
	}

	return records, changedByNormalization, nil
}

func buildEmailPoolRecord(line emailPoolLine, previous *emailPoolRecord, now string) (emailPoolRecord, bool, error) {
	record := emailPoolRecord{
		ID:           stableEmailPoolID(line.Email),
		Email:        line.Email,
		Password:     line.Password,
		ClientID:     line.ClientID,
		RefreshToken: line.RefreshToken,
		ExtraFields:  append([]string(nil), line.ExtraFields...),
	}
	changed := false

	if line.HasStatus {
		record.Status = line.Status
	} else if previous != nil {
		record.Status = previous.Status
	} else {
		record.Status = emailPoolStatusAvailable
		changed = true
	}

	if line.HasLeaseToken {
		record.LeaseToken = cloneStringPointer(line.LeaseToken)
	} else if previous != nil {
		record.LeaseToken = cloneStringPointer(previous.LeaseToken)
	}

	if line.HasLeasedAt {
		record.LeasedAt = cloneStringPointer(line.LeasedAt)
	} else if previous != nil {
		record.LeasedAt = cloneStringPointer(previous.LeasedAt)
	}

	if line.HasUsedAt {
		record.UsedAt = cloneStringPointer(line.UsedAt)
	} else if previous != nil {
		record.UsedAt = cloneStringPointer(previous.UsedAt)
	}
	record.CreatedAt = ""
	record.SourceUpdatedAt = ""
	record.UpdatedAt = ""

	if normalized, err := normalizeEmailPoolRecord(&record, now); err != nil {
		return emailPoolRecord{}, false, err
	} else if normalized {
		changed = true
	}

	return record, changed, nil
}

func normalizeEmailPoolRecord(record *emailPoolRecord, now string) (bool, error) {
	_ = now
	changed := false

	if record.Status == "" {
		record.Status = emailPoolStatusAvailable
		changed = true
	}

	normalizedStatus, err := normalizeEmailPoolStatus(record.Status)
	if err != nil {
		return false, err
	}
	if normalizedStatus != record.Status {
		record.Status = normalizedStatus
		changed = true
	}

	record.LeaseToken, changed = normalizeManagedPointer(record.LeaseToken, changed)
	record.LeasedAt, changed = normalizeManagedPointer(record.LeasedAt, changed)
	record.UsedAt, changed = normalizeManagedPointer(record.UsedAt, changed)

	switch record.Status {
	case emailPoolStatusAvailable:
		if record.LeaseToken != nil {
			record.LeaseToken = nil
			changed = true
		}
		if record.LeasedAt != nil {
			record.LeasedAt = nil
			changed = true
		}
		if record.UsedAt != nil {
			record.UsedAt = nil
			changed = true
		}
	case emailPoolStatusLeased:
		if record.LeaseToken == nil || record.LeasedAt == nil {
			record.Status = emailPoolStatusAvailable
			record.LeaseToken = nil
			record.LeasedAt = nil
			record.UsedAt = nil
			changed = true
		}
	case emailPoolStatusUsed:
		if record.LeaseToken != nil {
			record.LeaseToken = nil
			changed = true
		}
		if record.LeasedAt != nil {
			record.LeasedAt = nil
			changed = true
		}
	}

	return changed, nil
}

func normalizeManagedPointer(value *string, changed bool) (*string, bool) {
	if value == nil {
		return nil, changed
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, true
	}
	if trimmed == *value {
		return value, changed
	}
	return &trimmed, true
}

func diffEmailPoolRecords(previous []emailPoolRecord, current []emailPoolRecord) (int, int, int) {
	previousByEmail := make(map[string]emailPoolRecord, len(previous))
	for _, record := range previous {
		previousByEmail[normalizeEmailPoolKey(record.Email)] = cloneEmailPoolRecord(record)
	}

	inserted := 0
	updated := 0
	skipped := 0
	for _, record := range current {
		existing, ok := previousByEmail[normalizeEmailPoolKey(record.Email)]
		if !ok {
			inserted++
			continue
		}
		if sameEmailPoolRecord(existing, record) {
			skipped++
			continue
		}
		updated++
	}
	return inserted, updated, skipped
}

func sameEmailPoolSourceFields(left emailPoolRecord, right emailPoolRecord) bool {
	return left.Email == right.Email &&
		left.Password == right.Password &&
		left.ClientID == right.ClientID &&
		left.RefreshToken == right.RefreshToken &&
		sameStringSlice(left.ExtraFields, right.ExtraFields)
}

func sameEmailPoolRecord(left emailPoolRecord, right emailPoolRecord) bool {
	return left.ID == right.ID &&
		left.Email == right.Email &&
		left.Password == right.Password &&
		left.ClientID == right.ClientID &&
		left.RefreshToken == right.RefreshToken &&
		left.Status == right.Status &&
		sameStringPointer(left.LeaseToken, right.LeaseToken) &&
		sameStringPointer(left.LeasedAt, right.LeasedAt) &&
		sameStringPointer(left.UsedAt, right.UsedAt) &&
		sameStringSlice(left.ExtraFields, right.ExtraFields)
}

func sameStringPointer(left *string, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameStringSlice(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneEmailPoolRecord(record emailPoolRecord) emailPoolRecord {
	record.ExtraFields = append([]string(nil), record.ExtraFields...)
	record.LeaseToken = cloneStringPointer(record.LeaseToken)
	record.LeasedAt = cloneStringPointer(record.LeasedAt)
	record.UsedAt = cloneStringPointer(record.UsedAt)
	return record
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyStringPointerFromValue(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func normalizeEmailPoolKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (record emailPoolRecord) publicAccount() emailPoolPublicAccount {
	return emailPoolPublicAccount{
		ID:              record.ID,
		Email:           record.Email,
		Password:        record.Password,
		ClientID:        record.ClientID,
		RefreshToken:    record.RefreshToken,
		Status:          record.Status,
		LeaseToken:      cloneStringPointer(record.LeaseToken),
		LeasedAt:        cloneStringPointer(record.LeasedAt),
		UsedAt:          cloneStringPointer(record.UsedAt),
		SourceUpdatedAt: record.SourceUpdatedAt,
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
		ExtraFields:     append([]string(nil), record.ExtraFields...),
	}
}

func (store *emailPoolStore) saveRecordsUnlocked() error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o755); err != nil {
		return fmt.Errorf("创建 web_mail 数据目录失败: %w", err)
	}

	builder := strings.Builder{}
	for _, record := range store.records {
		builder.WriteString(serializeEmailPoolRecord(record))
		builder.WriteByte('\n')
	}

	tempFile, err := os.CreateTemp(filepath.Dir(store.path), filepath.Base(store.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("创建 web_mail 临时文件失败: %w", err)
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := tempFile.WriteString(builder.String()); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("写入 web_mail 临时文件失败: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("关闭 web_mail 临时文件失败: %w", err)
	}
	if err := os.Rename(tempPath, store.path); err != nil {
		return fmt.Errorf("替换 web_mail 文件失败: %w", err)
	}

	signature, err := getEmailPoolFileSignature(store.path)
	if err != nil {
		return fmt.Errorf("读取邮箱源文件签名失败: %w", err)
	}
	store.signature = signature
	return nil
}

func serializeEmailPoolRecord(record emailPoolRecord) string {
	fields := []string{
		record.Email,
		record.Password,
		record.ClientID,
		record.RefreshToken,
	}
	fields = append(fields, record.ExtraFields...)
	if record.LeaseToken != nil {
		fields = append(fields, emailPoolMetadataLeaseToken+":"+*record.LeaseToken)
	}
	if record.LeasedAt != nil {
		fields = append(fields, emailPoolMetadataLeasedAt+":"+*record.LeasedAt)
	}
	if record.UsedAt != nil {
		fields = append(fields, emailPoolMetadataUsedAt+":"+*record.UsedAt)
	}
	if record.Status != "" && record.Status != emailPoolStatusAvailable {
		fields = append(fields, emailPoolMetadataStatus+":"+record.Status)
	}
	return strings.Join(fields, emailPoolFieldDelimiter)
}

func (store *emailPoolStore) reloadIfChangedUnlocked(now string) error {
	currentSignature, err := getEmailPoolFileSignature(store.path)
	if err != nil {
		return fmt.Errorf("读取邮箱源文件签名失败: %w", err)
	}
	if sameEmailPoolFileSignature(store.signature, currentSignature) {
		return nil
	}

	loaded, changedByNormalization, err := store.loadRecordsUnlocked(now)
	if err != nil {
		return err
	}
	store.records = loaded
	store.signature = currentSignature
	if changedByNormalization {
		if err := store.saveRecordsUnlocked(); err != nil {
			return err
		}
	}
	return nil
}

func stableEmailPoolID(email string) int {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(normalizeEmailPoolKey(email)))
	value := int(hasher.Sum32() & 0x7fffffff)
	if value == 0 {
		return 1
	}
	return value
}

// getStats 返回邮箱池状态统计。
func (store *emailPoolStore) getStats(ctx context.Context) (emailPoolStats, error) {
	_ = ctx

	store.mu.Lock()
	defer store.mu.Unlock()

	stats := emailPoolStats{}
	for _, record := range store.records {
		stats.Total++
		switch record.Status {
		case emailPoolStatusAvailable:
			stats.Available++
		case emailPoolStatusLeased:
			stats.Leased++
		case emailPoolStatusUsed:
			stats.Used++
		}
	}
	return stats, nil
}

// leaseOne 原子租出一个可用邮箱账号。
// Why: 请求会并发命中 lease 接口，因此必须把“读最新文件状态 + 选中可用账号 + 回写租约”放进同一临界区，避免重复租出同一条记录。
func (store *emailPoolStore) leaseOne(ctx context.Context) (*emailPoolRecord, error) {
	_ = ctx

	var leasedRecord *emailPoolRecord
	err := store.withLockedFile(func() error {
		now := utcNowString()
		if err := store.reloadIfChangedUnlocked(now); err != nil {
			return err
		}

		index := -1
		for currentIndex := range store.records {
			if store.records[currentIndex].Status != emailPoolStatusAvailable {
				continue
			}
			if index == -1 || store.records[currentIndex].ID < store.records[index].ID {
				index = currentIndex
			}
		}
		if index < 0 {
			return nil
		}

		leaseToken, err := newLeaseToken()
		if err != nil {
			return err
		}
		record := &store.records[index]
		record.Status = emailPoolStatusLeased
		record.LeaseToken = &leaseToken
		record.LeasedAt = &now
		record.UsedAt = nil
		record.UpdatedAt = now

		if err := store.saveRecordsUnlocked(); err != nil {
			return err
		}
		copied := cloneEmailPoolRecord(*record)
		leasedRecord = &copied
		return nil
	})
	if err != nil {
		return nil, err
	}
	return leasedRecord, nil
}

// getAccount 按 ID 查询邮箱记录。
func (store *emailPoolStore) getAccount(ctx context.Context, accountID int) (*emailPoolRecord, error) {
	_ = ctx

	store.mu.Lock()
	defer store.mu.Unlock()

	for _, record := range store.records {
		if record.ID != accountID {
			continue
		}
		copied := cloneEmailPoolRecord(record)
		return &copied, nil
	}
	return nil, nil
}

// getAccountByEmail 按邮箱地址查询记录。
func (store *emailPoolStore) getAccountByEmail(ctx context.Context, email string) (*emailPoolRecord, error) {
	_ = ctx

	target := normalizeEmailPoolKey(email)
	store.mu.Lock()
	defer store.mu.Unlock()

	for _, record := range store.records {
		if normalizeEmailPoolKey(record.Email) != target {
			continue
		}
		copied := cloneEmailPoolRecord(record)
		return &copied, nil
	}
	return nil, nil
}

// markUsed 把租出的邮箱标记为已使用。
// Why: 注册成功后必须把邮箱永久移出可用池，否则后续流程会重复租到已经消费过的邮箱。
func (store *emailPoolStore) markUsed(ctx context.Context, accountID int, leaseToken string) (*emailPoolRecord, error) {
	_ = ctx

	var updatedRecord *emailPoolRecord
	err := store.withLockedFile(func() error {
		now := utcNowString()
		if err := store.reloadIfChangedUnlocked(now); err != nil {
			return err
		}

		index := findEmailPoolRecordIndexByID(store.records, accountID)
		if index < 0 {
			return nil
		}

		record := &store.records[index]
		if record.Status != emailPoolStatusLeased {
			return fmt.Errorf("当前状态为 %s，不能标记为已使用", record.Status)
		}
		if record.LeaseToken == nil || *record.LeaseToken != leaseToken {
			return errors.New("lease_token 不匹配，不能标记为已使用")
		}

		record.Status = emailPoolStatusUsed
		record.UsedAt = &now
		record.LeaseToken = nil
		record.LeasedAt = nil
		record.UpdatedAt = now

		if err := store.saveRecordsUnlocked(); err != nil {
			return err
		}
		copied := cloneEmailPoolRecord(*record)
		updatedRecord = &copied
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updatedRecord, nil
}

// returnToPool 把租出的邮箱归还到账号池。
// Why: 注册失败后及时归还邮箱，才能让后续 worker 继续消费这批尚未成功注册的地址。
func (store *emailPoolStore) returnToPool(ctx context.Context, accountID int, leaseToken string) (*emailPoolRecord, error) {
	_ = ctx

	var updatedRecord *emailPoolRecord
	err := store.withLockedFile(func() error {
		now := utcNowString()
		if err := store.reloadIfChangedUnlocked(now); err != nil {
			return err
		}

		index := findEmailPoolRecordIndexByID(store.records, accountID)
		if index < 0 {
			return nil
		}

		record := &store.records[index]
		if record.Status != emailPoolStatusLeased {
			return fmt.Errorf("当前状态为 %s，不能归还", record.Status)
		}
		if record.LeaseToken == nil || *record.LeaseToken != leaseToken {
			return errors.New("lease_token 不匹配，不能归还")
		}

		record.Status = emailPoolStatusAvailable
		record.LeaseToken = nil
		record.LeasedAt = nil
		record.UsedAt = nil
		record.UpdatedAt = now

		if err := store.saveRecordsUnlocked(); err != nil {
			return err
		}
		copied := cloneEmailPoolRecord(*record)
		updatedRecord = &copied
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updatedRecord, nil
}

func findEmailPoolRecordIndexByID(records []emailPoolRecord, accountID int) int {
	for index := range records {
		if records[index].ID == accountID {
			return index
		}
	}
	return -1
}

// reclaimExpiredLeases 回收超时未确认的租约。
// Why: 调用方崩溃后邮箱可能长期卡在 leased 状态，自动回收是避免账号池被占死的关键兜底机制。
func (store *emailPoolStore) reclaimExpiredLeases(ctx context.Context, leaseTimeoutSeconds int) (emailPoolReclaimResult, error) {
	_ = ctx

	result := emailPoolReclaimResult{
		ReclaimedIDs:        []int{},
		CheckedAt:           utcNowString(),
		LeaseTimeoutSeconds: leaseTimeoutSeconds,
	}

	err := store.withLockedFile(func() error {
		if err := store.reloadIfChangedUnlocked(result.CheckedAt); err != nil {
			return err
		}

		expireBefore := time.Now().UTC().Add(-time.Duration(leaseTimeoutSeconds) * time.Second)
		changed := false
		for index := range store.records {
			record := &store.records[index]
			if record.Status != emailPoolStatusLeased || record.LeasedAt == nil {
				continue
			}

			leasedAt, err := time.Parse(time.RFC3339Nano, *record.LeasedAt)
			if err != nil || leasedAt.After(expireBefore) {
				continue
			}

			record.Status = emailPoolStatusAvailable
			record.LeaseToken = nil
			record.LeasedAt = nil
			record.UsedAt = nil
			record.UpdatedAt = result.CheckedAt
			result.ReclaimedIDs = append(result.ReclaimedIDs, record.ID)
			changed = true
		}

		result.ReclaimedCount = len(result.ReclaimedIDs)
		if !changed {
			return nil
		}
		return store.saveRecordsUnlocked()
	})
	if err != nil {
		return emailPoolReclaimResult{}, err
	}

	return result, nil
}

// newUpstreamMailClient 创建上游邮件接口客户端。
func newUpstreamMailClient(baseURL string, timeout time.Duration) *upstreamMailClient {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &upstreamMailClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// fetchLatestMail 根据 refresh_token/client_id/email 查询最新邮件。
// Why: web_mail 服务的本质是把邮箱租约和取信逻辑组装成一个本地 HTTP 服务，因此上游取信必须由服务端统一代理。
func (client *upstreamMailClient) fetchLatestMail(ctx context.Context, record emailPoolRecord, mailbox string) (map[string]any, error) {
	normalizedMailbox, err := normalizeEmailPoolMailbox(mailbox)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("refresh_token", record.RefreshToken)
	query.Set("client_id", record.ClientID)
	query.Set("email", record.Email)
	query.Set("mailbox", normalizedMailbox)
	query.Set("response_type", "json")

	requestURL := client.baseURL + "/api/mail-new?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建上游邮件请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "web_mail/1.0")

	response, err := client.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求上游邮件接口失败: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("读取上游邮件响应失败: %w", err)
	}
	if response.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("上游邮件接口返回 HTTP %d: %s", response.StatusCode, truncateForLog(body, 300))
	}

	payload := map[string]any{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("上游邮件接口返回了非 JSON 内容: %s", truncateForLog(body, 300))
	}
	return payload, nil
}

func truncateForLog(payload []byte, limit int) string {
	if len(payload) <= limit {
		return string(payload)
	}
	return string(payload[:limit])
}

// newEmailPoolService 构造带自动同步和租约回收缓存的服务实例。
func newEmailPoolService(store *emailPoolStore, emailsFile string, mailClient *upstreamMailClient, leaseTimeoutSeconds int, logger *log.Logger) *emailPoolService {
	// Why: 单测和局部复用场景不一定会显式注入 logger，这里统一兜底到 discard，避免关键日志调用散落空指针判断。
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &emailPoolService{
		store:               store,
		emailsFile:          emailsFile,
		mailClient:          mailClient,
		leaseTimeoutSeconds: leaseTimeoutSeconds,
		logger:              logger,
	}
}

func getEmailPoolFileSignature(path string) (*emailPoolFileSignature, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &emailPoolFileSignature{
		ModTimeNS: stat.ModTime().UnixNano(),
		Size:      stat.Size(),
	}, nil
}

func sameEmailPoolFileSignature(left *emailPoolFileSignature, right *emailPoolFileSignature) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.ModTimeNS == right.ModTimeNS && left.Size == right.Size
}

// ensureSynced 在请求前按需执行 emails.txt 自动同步。
// Why: 邮箱文件可能被外部脚本持续追加或人工编辑，因此服务端必须在处理请求前懒同步，避免长驻进程拿到过期数据。
func (service *emailPoolService) ensureSynced(ctx context.Context, force bool) (emailPoolSyncResult, error) {
	currentSignature, err := getEmailPoolFileSignature(service.emailsFile)
	if err != nil {
		return emailPoolSyncResult{}, fmt.Errorf("读取邮箱源文件签名失败: %w", err)
	}

	if !force && sameEmailPoolFileSignature(service.fileSignature, currentSignature) {
		if service.lastSyncResult.File != "" {
			return service.lastSyncResult, nil
		}
		return emailPoolSyncResult{
			File:     service.emailsFile,
			SyncedAt: "",
			Reason:   "no_change",
		}, nil
	}

	service.syncMu.Lock()
	defer service.syncMu.Unlock()

	currentSignature, err = getEmailPoolFileSignature(service.emailsFile)
	if err != nil {
		return emailPoolSyncResult{}, fmt.Errorf("读取邮箱源文件签名失败: %w", err)
	}
	if !force && sameEmailPoolFileSignature(service.fileSignature, currentSignature) {
		if service.lastSyncResult.File != "" {
			return service.lastSyncResult, nil
		}
		return emailPoolSyncResult{
			File:     service.emailsFile,
			SyncedAt: "",
			Reason:   "no_change",
		}, nil
	}

	result, err := service.store.syncFromFile(ctx, service.emailsFile)
	if err != nil {
		return emailPoolSyncResult{}, err
	}

	latestSignature, err := getEmailPoolFileSignature(service.emailsFile)
	if err != nil {
		return emailPoolSyncResult{}, fmt.Errorf("读取邮箱源文件签名失败: %w", err)
	}
	service.fileSignature = latestSignature
	service.lastSyncResult = result
	return result, nil
}

// ensureReclaimed 以最小检查间隔执行租约回收。
// Why: 高频请求下每次都扫描 leased 记录会放大文件 IO，因此这里显式做节流。
func (service *emailPoolService) ensureReclaimed(ctx context.Context, force bool) (emailPoolReclaimResult, error) {
	if !force && !service.lastReclaimCheckAt.IsZero() && time.Since(service.lastReclaimCheckAt) < emailPoolReclaimCheckInterval {
		return service.lastReclaimResult, nil
	}

	service.reclaimMu.Lock()
	defer service.reclaimMu.Unlock()

	if !force && !service.lastReclaimCheckAt.IsZero() && time.Since(service.lastReclaimCheckAt) < emailPoolReclaimCheckInterval {
		return service.lastReclaimResult, nil
	}

	result, err := service.store.reclaimExpiredLeases(ctx, service.leaseTimeoutSeconds)
	if err != nil {
		return emailPoolReclaimResult{}, err
	}
	if result.ReclaimedCount > 0 {
		service.logf("web_mail 已回收超时租约: reclaimed=%d ids=%v lease_timeout_seconds=%d", result.ReclaimedCount, result.ReclaimedIDs, result.LeaseTimeoutSeconds)
	}
	service.lastReclaimResult = result
	service.lastReclaimCheckAt = time.Now()
	return result, nil
}

func (service *emailPoolService) prepareRequestData(ctx context.Context) error {
	if _, err := service.ensureSynced(ctx, false); err != nil {
		return err
	}
	if _, err := service.ensureReclaimed(ctx, false); err != nil {
		return err
	}
	return nil
}

// ServeHTTP 实现邮箱池 HTTP 接口。
func (service *emailPoolService) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		service.handleGet(writer, request)
	case http.MethodPost:
		service.handlePost(writer, request)
	default:
		service.respondError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
	}
}

func (service *emailPoolService) handleGet(writer http.ResponseWriter, request *http.Request) {
	path := normalizeEmailPoolPath(request.URL.Path)
	if path == "/health" {
		service.respondJSON(writer, http.StatusOK, map[string]any{
			"ok":      true,
			"service": "email-pool",
		})
		return
	}

	if err := service.prepareRequestData(request.Context()); err != nil {
		service.respondUnexpected(writer, request, err)
		return
	}

	switch {
	case path == "/api/email-pool/stats":
		stats, err := service.store.getStats(request.Context())
		if err != nil {
			service.respondUnexpected(writer, request, err)
			return
		}
		service.respondEnvelope(writer, http.StatusOK, stats)
	case path == "/api/email-pool/accounts/by-email/latest":
		email := strings.TrimSpace(request.URL.Query().Get("email"))
		mailbox := request.URL.Query().Get("mailbox")
		service.handleLatestMailByEmail(writer, request, email, mailbox)
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/latest"):
		accountID, err := extractEmailPoolAccountID(path, "/latest")
		if err != nil {
			service.logRequestFailure(request, http.StatusBadRequest, "latest 查询失败: 非法账号 ID")
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		mailbox := request.URL.Query().Get("mailbox")
		service.handleLatestMailByID(writer, request, accountID, mailbox)
	default:
		service.respondError(writer, http.StatusNotFound, "接口不存在")
	}
}

func (service *emailPoolService) handlePost(writer http.ResponseWriter, request *http.Request) {
	path := normalizeEmailPoolPath(request.URL.Path)
	payload, err := readOptionalJSONBody(request.Body)
	if err != nil {
		service.logRequestFailure(request, http.StatusBadRequest, "请求体不是合法 JSON")
		service.respondError(writer, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	if path != "/api/email-pool/sync" {
		if err := service.prepareRequestData(request.Context()); err != nil {
			service.respondUnexpected(writer, request, err)
			return
		}
	}

	switch {
	case path == "/api/email-pool/sync":
		syncResult, err := service.ensureSynced(request.Context(), true)
		if err != nil {
			service.respondUnexpected(writer, request, err)
			return
		}
		reclaimResult, err := service.ensureReclaimed(request.Context(), true)
		if err != nil {
			service.respondUnexpected(writer, request, err)
			return
		}
		service.logf(
			"web_mail 手动同步完成: inserted=%d updated=%d skipped=%d reclaimed=%d file=%s",
			syncResult.Inserted,
			syncResult.Updated,
			syncResult.Skipped,
			reclaimResult.ReclaimedCount,
			syncResult.File,
		)
		service.respondEnvelope(writer, http.StatusOK, emailPoolSyncEnvelopeData{
			Sync:    syncResult,
			Reclaim: reclaimResult,
		})
	case path == "/api/email-pool/lease":
		record, err := service.store.leaseOne(request.Context())
		if err != nil {
			service.respondUnexpected(writer, request, err)
			return
		}
		if record == nil {
			service.logRequestFailure(request, http.StatusNotFound, "租号失败: 当前没有可用邮箱账号")
			service.respondError(writer, http.StatusNotFound, "当前没有可用邮箱账号")
			return
		}
		service.logRequestSuccess(request, "租出邮箱 account_id=%d email=%s", record.ID, record.Email)
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case path == "/api/email-pool/accounts/by-email/latest":
		email := strings.TrimSpace(fmt.Sprintf("%v", payload["email"]))
		mailbox := fmt.Sprintf("%v", payload["mailbox"])
		service.handleLatestMailByEmail(writer, request, email, mailbox)
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/mark-used"):
		accountID, err := extractEmailPoolAccountID(path, "/mark-used")
		if err != nil {
			service.logRequestFailure(request, http.StatusBadRequest, "标记已使用失败: 非法账号 ID")
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		leaseToken := strings.TrimSpace(fmt.Sprintf("%v", payload["lease_token"]))
		if leaseToken == "" {
			service.logRequestFailure(request, http.StatusBadRequest, "标记已使用失败: account_id=%d lease_token 缺失", accountID)
			service.respondError(writer, http.StatusBadRequest, "lease_token 必填")
			return
		}
		record, err := service.store.markUsed(request.Context(), accountID, leaseToken)
		if err != nil {
			if isLeaseConflict(err) {
				service.logRequestFailure(request, http.StatusConflict, "标记已使用冲突: account_id=%d err=%v", accountID, err)
				service.respondError(writer, http.StatusConflict, err.Error())
				return
			}
			service.respondUnexpected(writer, request, err)
			return
		}
		if record == nil {
			service.logRequestFailure(request, http.StatusNotFound, "标记已使用失败: account_id=%d 不存在", accountID)
			service.respondError(writer, http.StatusNotFound, "账号不存在")
			return
		}
		service.logRequestSuccess(request, "标记邮箱已使用: account_id=%d email=%s", record.ID, record.Email)
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/return"):
		accountID, err := extractEmailPoolAccountID(path, "/return")
		if err != nil {
			service.logRequestFailure(request, http.StatusBadRequest, "归还邮箱失败: 非法账号 ID")
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		leaseToken := strings.TrimSpace(fmt.Sprintf("%v", payload["lease_token"]))
		if leaseToken == "" {
			service.logRequestFailure(request, http.StatusBadRequest, "归还邮箱失败: account_id=%d lease_token 缺失", accountID)
			service.respondError(writer, http.StatusBadRequest, "lease_token 必填")
			return
		}
		record, err := service.store.returnToPool(request.Context(), accountID, leaseToken)
		if err != nil {
			if isLeaseConflict(err) {
				service.logRequestFailure(request, http.StatusConflict, "归还邮箱冲突: account_id=%d err=%v", accountID, err)
				service.respondError(writer, http.StatusConflict, err.Error())
				return
			}
			service.respondUnexpected(writer, request, err)
			return
		}
		if record == nil {
			service.logRequestFailure(request, http.StatusNotFound, "归还邮箱失败: account_id=%d 不存在", accountID)
			service.respondError(writer, http.StatusNotFound, "账号不存在")
			return
		}
		service.logRequestSuccess(request, "归还邮箱成功: account_id=%d email=%s", record.ID, record.Email)
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/latest"):
		accountID, err := extractEmailPoolAccountID(path, "/latest")
		if err != nil {
			service.logRequestFailure(request, http.StatusBadRequest, "latest 查询失败: 非法账号 ID")
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		mailbox := fmt.Sprintf("%v", payload["mailbox"])
		service.handleLatestMailByID(writer, request, accountID, mailbox)
	default:
		service.respondError(writer, http.StatusNotFound, "接口不存在")
	}
}

func isLeaseConflict(err error) bool {
	return strings.Contains(err.Error(), "lease_token") || strings.Contains(err.Error(), "当前状态")
}

func normalizeEmailPoolPath(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func extractEmailPoolAccountID(path string, suffix string) (int, error) {
	prefix := "/api/email-pool/accounts/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return 0, errors.New("invalid account path")
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	raw = strings.Trim(raw, "/")
	return strconv.Atoi(raw)
}

func readOptionalJSONBody(body io.ReadCloser) (map[string]any, error) {
	defer func() {
		_ = body.Close()
	}()
	rawBody, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(rawBody))) == 0 {
		return map[string]any{}, nil
	}

	payload := map[string]any{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func normalizeEmailPoolMailbox(mailbox string) (string, error) {
	if strings.TrimSpace(mailbox) == "" {
		return "INBOX", nil
	}
	if _, ok := emailPoolValidMailboxes[mailbox]; ok {
		return mailbox, nil
	}
	return "", fmt.Errorf("mailbox 仅支持: [INBOX Junk]")
}

func (service *emailPoolService) handleLatestMailByID(writer http.ResponseWriter, request *http.Request, accountID int, mailbox string) {
	normalizedMailbox, err := normalizeEmailPoolMailbox(mailbox)
	if err != nil {
		service.logRequestFailure(request, http.StatusBadRequest, "获取最新邮件失败: account_id=%d mailbox=%q err=%v", accountID, mailbox, err)
		service.respondError(writer, http.StatusBadRequest, err.Error())
		return
	}

	record, err := service.store.getAccount(request.Context(), accountID)
	if err != nil {
		service.respondUnexpected(writer, request, err)
		return
	}
	if record == nil {
		service.logRequestFailure(request, http.StatusNotFound, "获取最新邮件失败: account_id=%d 不存在", accountID)
		service.respondError(writer, http.StatusNotFound, "账号不存在")
		return
	}

	latestMail, err := service.mailClient.fetchLatestMail(request.Context(), *record, normalizedMailbox)
	if err != nil {
		service.logRequestFailure(request, http.StatusBadGateway, "获取最新邮件失败: account_id=%d email=%s mailbox=%s err=%v", accountID, record.Email, normalizedMailbox, err)
		service.respondError(writer, http.StatusBadGateway, err.Error())
		return
	}
	service.logRequestSuccess(request, "获取最新邮件成功: account_id=%d email=%s mailbox=%s", record.ID, record.Email, normalizedMailbox)
	service.respondEnvelope(writer, http.StatusOK, map[string]any{
		"account":     record.publicAccount(),
		"mailbox":     normalizedMailbox,
		"latest_mail": latestMail,
	})
}

func (service *emailPoolService) handleLatestMailByEmail(writer http.ResponseWriter, request *http.Request, email string, mailbox string) {
	if email == "" {
		service.logRequestFailure(request, http.StatusBadRequest, "获取最新邮件失败: email 缺失")
		service.respondError(writer, http.StatusBadRequest, "email 必填")
		return
	}

	normalizedMailbox, err := normalizeEmailPoolMailbox(mailbox)
	if err != nil {
		service.logRequestFailure(request, http.StatusBadRequest, "获取最新邮件失败: email=%s mailbox=%q err=%v", email, mailbox, err)
		service.respondError(writer, http.StatusBadRequest, err.Error())
		return
	}

	record, err := service.store.getAccountByEmail(request.Context(), email)
	if err != nil {
		service.respondUnexpected(writer, request, err)
		return
	}
	if record == nil {
		service.logRequestFailure(request, http.StatusNotFound, "获取最新邮件失败: email=%s 不存在", email)
		service.respondError(writer, http.StatusNotFound, "邮箱账号不存在")
		return
	}

	latestMail, err := service.mailClient.fetchLatestMail(request.Context(), *record, normalizedMailbox)
	if err != nil {
		service.logRequestFailure(request, http.StatusBadGateway, "获取最新邮件失败: account_id=%d email=%s mailbox=%s err=%v", record.ID, record.Email, normalizedMailbox, err)
		service.respondError(writer, http.StatusBadGateway, err.Error())
		return
	}
	service.logRequestSuccess(request, "获取最新邮件成功: account_id=%d email=%s mailbox=%s", record.ID, record.Email, normalizedMailbox)
	service.respondEnvelope(writer, http.StatusOK, map[string]any{
		"account":     record.publicAccount(),
		"mailbox":     normalizedMailbox,
		"latest_mail": latestMail,
	})
}

func (service *emailPoolService) respondEnvelope(writer http.ResponseWriter, statusCode int, data any) {
	service.respondJSON(writer, statusCode, emailPoolEnvelope{
		OK:   true,
		Data: data,
	})
}

func (service *emailPoolService) respondError(writer http.ResponseWriter, statusCode int, message string) {
	service.respondJSON(writer, statusCode, emailPoolEnvelope{
		OK:    false,
		Error: message,
	})
}

// logRequestSuccess 统一记录关键接口的成功日志，便于从服务端快速回放租约和取信主链路。
func (service *emailPoolService) logRequestSuccess(request *http.Request, format string, args ...any) {
	service.logf("web_mail 请求成功: method=%s path=%s detail=%s", request.Method, normalizeEmailPoolPath(request.URL.Path), fmt.Sprintf(format, args...))
}

// logRequestFailure 统一记录关键接口的失败日志，避免 HTTP 已返回但服务端上下文丢失。
func (service *emailPoolService) logRequestFailure(request *http.Request, statusCode int, format string, args ...any) {
	service.logf("web_mail 请求失败: method=%s path=%s status=%d detail=%s", request.Method, normalizeEmailPoolPath(request.URL.Path), statusCode, fmt.Sprintf(format, args...))
}

func (service *emailPoolService) logf(format string, args ...any) {
	if service.logger != nil {
		service.logger.Printf(format, args...)
	}
}

func (service *emailPoolService) respondUnexpected(writer http.ResponseWriter, request *http.Request, err error) {
	service.logRequestFailure(request, http.StatusInternalServerError, "服务内部错误: %v", err)
	service.respondError(writer, http.StatusInternalServerError, "服务内部错误")
}

func (service *emailPoolService) respondJSON(writer http.ResponseWriter, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(writer, `{"ok":false,"error":"服务内部错误"}`, http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(statusCode)
	_, _ = writer.Write(body)
}

func newLeaseToken() (string, error) {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("生成租约令牌失败: %w", err)
	}
	return hex.EncodeToString(randomBytes), nil
}

func utcNowString() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
