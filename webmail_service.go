package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultMailAPIBaseURL             = "https://www.appleemail.top"
	defaultWebMailLeaseTimeoutSeconds = 600
	emailPoolStatusAvailable          = "available"
	emailPoolStatusLeased             = "leased"
	emailPoolStatusUsed               = "used"
	emailPoolBusyRetryCount           = 5
	emailPoolBusyRetryDelay           = 200 * time.Millisecond
	emailPoolReclaimCheckInterval     = 3 * time.Second
	emailPoolSelectColumns            = "id, email, password, client_id, refresh_token, status, lease_token, leased_at, used_at, source_updated_at, created_at, updated_at, extra_fields_json"
)

var emailPoolValidMailboxes = map[string]struct{}{
	"INBOX": {},
	"Junk":  {},
}

// emailPoolLine 表示 emails.txt 中解析出的单行邮箱记录。
// Why: 先把文本行转换成稳定结构，再做数据库同步，能把“文件格式错误”和“状态写入失败”明确分层。
type emailPoolLine struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
	ExtraFields  []string
}

// emailPoolRecord 表示 SQLite 中的一条邮箱记录。
// Why: 服务端既要对外返回公共字段，也要保留内部状态字段，因此先收敛为一个完整记录模型更容易维护。
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
	ExtraFieldsJSON string
}

// emailPoolPublicAccount 是对外暴露的邮箱池账号结构。
// Why: 对外接口要保留来源服务的字段形状，避免当前 Go 客户端和其他脚本因为返回体变化而失配。
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

type emailPoolScanner interface {
	Scan(dest ...any) error
}

type emailPoolQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// emailPoolStore 负责邮箱池状态的 SQLite 持久化。
// Why: web_mail 的核心价值在于“租约状态机 + 文件同步”，把这部分收拢到存储层可以避免 HTTP 层掺杂数据库细节。
type emailPoolStore struct {
	db      *sql.DB
	writeMu sync.Mutex
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
	syncMu              sync.Mutex
	fileSignature       *emailPoolFileSignature
	lastSyncResult      emailPoolSyncResult
	reclaimMu           sync.Mutex
	lastReclaimResult   emailPoolReclaimResult
	lastReclaimCheckAt  time.Time
}

// runWebMailServer 启动当前仓库内置的 web_mail HTTP 服务。
// Why: 用户要求独立模式启动，因此这里明确提供单独 server 入口，而不是把服务偷偷嵌进注册主流程。
func runWebMailServer(parent context.Context, cfg config, logger *log.Logger) error {
	store, err := newEmailPoolStore(cfg.webMailDBPath)
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
// Why: 来源文件使用 ---- 分隔，且后续可能继续追加附加列，因此这里显式保留 extra_fields 防止信息丢失。
func parseEmailPoolLine(line string) (*emailPoolLine, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}

	parts := strings.Split(trimmed, "----")
	if len(parts) < 4 {
		return nil, fmt.Errorf("邮箱数据字段不足 4 列: %s", trimmed)
	}

	record := &emailPoolLine{
		Email:        strings.TrimSpace(parts[0]),
		Password:     strings.TrimSpace(parts[1]),
		ClientID:     strings.TrimSpace(parts[2]),
		RefreshToken: strings.TrimSpace(parts[3]),
	}
	for _, field := range parts[4:] {
		record.ExtraFields = append(record.ExtraFields, strings.TrimSpace(field))
	}
	return record, nil
}

// newEmailPoolStore 初始化邮箱池 SQLite 数据库。
// Why: 这里把目录创建、连接池和 PRAGMA 初始化一次性做完，避免后续每个调用点重复拼装数据库状态。
func newEmailPoolStore(dbPath string) (*emailPoolStore, error) {
	directory := filepath.Dir(dbPath)
	if directory != "." && directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("创建 web_mail 数据目录失败: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 web_mail 数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &emailPoolStore{db: db}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close 关闭 SQLite 连接池。
func (store *emailPoolStore) Close() error {
	return store.db.Close()
}

func (store *emailPoolStore) initialize() error {
	statements := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		`
		CREATE TABLE IF NOT EXISTS email_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL,
			client_id TEXT NOT NULL,
			refresh_token TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'available',
			lease_token TEXT,
			leased_at TEXT,
			used_at TEXT,
			source_updated_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			extra_fields_json TEXT NOT NULL DEFAULT '[]'
		)
		`,
		`
		CREATE INDEX IF NOT EXISTS idx_email_accounts_status_id
		ON email_accounts(status, id)
		`,
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			return fmt.Errorf("初始化 web_mail 数据库失败: %w", err)
		}
	}
	return nil
}

func runEmailPoolWithRetry[T any](operation string, callback func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 1; attempt <= emailPoolBusyRetryCount; attempt++ {
		result, err := callback()
		if err == nil {
			return result, nil
		}
		if !isSQLiteBusyError(err) {
			return zero, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt) * emailPoolBusyRetryDelay)
	}
	return zero, fmt.Errorf("SQLite 并发繁忙，操作失败: %s: %w", operation, lastErr)
}

func isSQLiteBusyError(err error) bool {
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "database is locked") ||
		strings.Contains(lower, "database table is locked") ||
		strings.Contains(lower, "sqlite_busy")
}

func utcNowString() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copied := value.String
	return &copied
}

func scanEmailPoolRecord(scanner emailPoolScanner) (emailPoolRecord, error) {
	record := emailPoolRecord{}
	var leaseToken sql.NullString
	var leasedAt sql.NullString
	var usedAt sql.NullString
	if err := scanner.Scan(
		&record.ID,
		&record.Email,
		&record.Password,
		&record.ClientID,
		&record.RefreshToken,
		&record.Status,
		&leaseToken,
		&leasedAt,
		&usedAt,
		&record.SourceUpdatedAt,
		&record.CreatedAt,
		&record.UpdatedAt,
		&record.ExtraFieldsJSON,
	); err != nil {
		return emailPoolRecord{}, err
	}
	record.LeaseToken = nullableString(leaseToken)
	record.LeasedAt = nullableString(leasedAt)
	record.UsedAt = nullableString(usedAt)
	return record, nil
}

func fetchEmailPoolRecord(ctx context.Context, queryer emailPoolQueryer, query string, args ...any) (*emailPoolRecord, error) {
	record, err := scanEmailPoolRecord(queryer.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

func (record emailPoolRecord) publicAccount() emailPoolPublicAccount {
	extraFields := []string{}
	if record.ExtraFieldsJSON != "" {
		_ = json.Unmarshal([]byte(record.ExtraFieldsJSON), &extraFields)
	}
	return emailPoolPublicAccount{
		ID:              record.ID,
		Email:           record.Email,
		Password:        record.Password,
		ClientID:        record.ClientID,
		RefreshToken:    record.RefreshToken,
		Status:          record.Status,
		LeaseToken:      record.LeaseToken,
		LeasedAt:        record.LeasedAt,
		UsedAt:          record.UsedAt,
		SourceUpdatedAt: record.SourceUpdatedAt,
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
		ExtraFields:     extraFields,
	}
}

// syncFromFile 把 emails.txt 中的邮箱数据同步到 SQLite。
// Why: 这里按 email 做增量同步而不是全量覆盖，是为了保留租约状态和历史 used 痕迹，避免文件覆盖把运行状态清空。
func (store *emailPoolStore) syncFromFile(ctx context.Context, emailsFile string) (emailPoolSyncResult, error) {
	lines, err := os.ReadFile(emailsFile)
	if err != nil {
		return emailPoolSyncResult{}, fmt.Errorf("读取邮箱源文件失败: %w", err)
	}

	parsedLines := make([]emailPoolLine, 0)
	for _, line := range strings.Split(string(lines), "\n") {
		parsed, err := parseEmailPoolLine(line)
		if err != nil {
			return emailPoolSyncResult{}, err
		}
		if parsed == nil {
			continue
		}
		parsedLines = append(parsedLines, *parsed)
	}

	return runEmailPoolWithRetry("sync_from_file", func() (emailPoolSyncResult, error) {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return emailPoolSyncResult{}, err
		}
		defer func() {
			_ = tx.Rollback()
		}()

		result := emailPoolSyncResult{
			File:     emailsFile,
			SyncedAt: utcNowString(),
		}
		for _, parsed := range parsedLines {
			current, err := fetchEmailPoolRecord(
				ctx,
				tx,
				"SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE email = ?",
				parsed.Email,
			)
			if err != nil {
				return emailPoolSyncResult{}, err
			}

			extraFieldsJSON, err := json.Marshal(parsed.ExtraFields)
			if err != nil {
				return emailPoolSyncResult{}, fmt.Errorf("序列化 extra_fields 失败: %w", err)
			}

			if current == nil {
				_, err = tx.ExecContext(
					ctx,
					`
					INSERT INTO email_accounts (
						email, password, client_id, refresh_token, status,
						source_updated_at, created_at, updated_at, extra_fields_json
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
					`,
					parsed.Email,
					parsed.Password,
					parsed.ClientID,
					parsed.RefreshToken,
					emailPoolStatusAvailable,
					result.SyncedAt,
					result.SyncedAt,
					result.SyncedAt,
					string(extraFieldsJSON),
				)
				if err != nil {
					return emailPoolSyncResult{}, err
				}
				result.Inserted++
				continue
			}

			if current.Password == parsed.Password &&
				current.ClientID == parsed.ClientID &&
				current.RefreshToken == parsed.RefreshToken &&
				current.ExtraFieldsJSON == string(extraFieldsJSON) {
				if _, err := tx.ExecContext(
					ctx,
					"UPDATE email_accounts SET source_updated_at = ? WHERE email = ?",
					result.SyncedAt,
					parsed.Email,
				); err != nil {
					return emailPoolSyncResult{}, err
				}
				result.Skipped++
				continue
			}

			if _, err := tx.ExecContext(
				ctx,
				`
				UPDATE email_accounts
				SET password = ?, client_id = ?, refresh_token = ?, source_updated_at = ?, updated_at = ?, extra_fields_json = ?
				WHERE email = ?
				`,
				parsed.Password,
				parsed.ClientID,
				parsed.RefreshToken,
				result.SyncedAt,
				result.SyncedAt,
				string(extraFieldsJSON),
				parsed.Email,
			); err != nil {
				return emailPoolSyncResult{}, err
			}
			result.Updated++
		}

		if err := tx.Commit(); err != nil {
			return emailPoolSyncResult{}, err
		}
		return result, nil
	})
}

// getStats 返回邮箱池状态统计。
func (store *emailPoolStore) getStats(ctx context.Context) (emailPoolStats, error) {
	return runEmailPoolWithRetry("get_stats", func() (emailPoolStats, error) {
		rows, err := store.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM email_accounts GROUP BY status")
		if err != nil {
			return emailPoolStats{}, err
		}
		defer func() {
			_ = rows.Close()
		}()

		stats := emailPoolStats{}
		for rows.Next() {
			var status string
			var count int
			if err := rows.Scan(&status, &count); err != nil {
				return emailPoolStats{}, err
			}
			stats.Total += count
			switch status {
			case emailPoolStatusAvailable:
				stats.Available = count
			case emailPoolStatusLeased:
				stats.Leased = count
			case emailPoolStatusUsed:
				stats.Used = count
			}
		}
		return stats, rows.Err()
	})
}

// leaseOne 原子租出一个可用邮箱账号。
// Why: 注册线程会并发取号，必须把“查询可用账号”和“标记已租出”放在同一事务内完成，避免重复租出同一邮箱。
func (store *emailPoolStore) leaseOne(ctx context.Context) (*emailPoolRecord, error) {
	return runEmailPoolWithRetry("lease_one", func() (*emailPoolRecord, error) {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = tx.Rollback()
		}()

		record, err := fetchEmailPoolRecord(
			ctx,
			tx,
			"SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE status = ? ORDER BY id ASC LIMIT 1",
			emailPoolStatusAvailable,
		)
		if err != nil || record == nil {
			return record, err
		}

		leaseToken, err := newLeaseToken()
		if err != nil {
			return nil, err
		}
		now := utcNowString()
		if _, err := tx.ExecContext(
			ctx,
			"UPDATE email_accounts SET status = ?, lease_token = ?, leased_at = ?, updated_at = ? WHERE id = ?",
			emailPoolStatusLeased,
			leaseToken,
			now,
			now,
			record.ID,
		); err != nil {
			return nil, err
		}

		updated, err := fetchEmailPoolRecord(ctx, tx, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", record.ID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// getAccount 按 ID 查询邮箱记录。
func (store *emailPoolStore) getAccount(ctx context.Context, accountID int) (*emailPoolRecord, error) {
	return runEmailPoolWithRetry("get_account", func() (*emailPoolRecord, error) {
		return fetchEmailPoolRecord(ctx, store.db, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", accountID)
	})
}

// getAccountByEmail 按邮箱地址查询记录。
func (store *emailPoolStore) getAccountByEmail(ctx context.Context, email string) (*emailPoolRecord, error) {
	return runEmailPoolWithRetry("get_account_by_email", func() (*emailPoolRecord, error) {
		return fetchEmailPoolRecord(ctx, store.db, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE email = ?", email)
	})
}

// markUsed 把租出的邮箱标记为已使用。
// Why: 注册成功后必须把邮箱永久移出可用池，否则后续流程会重复租到已经消费过的邮箱。
func (store *emailPoolStore) markUsed(ctx context.Context, accountID int, leaseToken string) (*emailPoolRecord, error) {
	return runEmailPoolWithRetry("mark_used", func() (*emailPoolRecord, error) {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = tx.Rollback()
		}()

		record, err := fetchEmailPoolRecord(ctx, tx, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", accountID)
		if err != nil || record == nil {
			return record, err
		}
		if record.Status != emailPoolStatusLeased {
			return nil, fmt.Errorf("当前状态为 %s，不能标记为已使用", record.Status)
		}
		if record.LeaseToken == nil || *record.LeaseToken != leaseToken {
			return nil, errors.New("lease_token 不匹配，不能标记为已使用")
		}

		now := utcNowString()
		if _, err := tx.ExecContext(
			ctx,
			"UPDATE email_accounts SET status = ?, used_at = ?, lease_token = NULL, leased_at = NULL, updated_at = ? WHERE id = ?",
			emailPoolStatusUsed,
			now,
			now,
			accountID,
		); err != nil {
			return nil, err
		}

		updated, err := fetchEmailPoolRecord(ctx, tx, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", accountID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// returnToPool 把租出的邮箱归还到账号池。
// Why: 注册失败后及时归还邮箱，才能让后续 worker 继续消费这批尚未成功注册的地址。
func (store *emailPoolStore) returnToPool(ctx context.Context, accountID int, leaseToken string) (*emailPoolRecord, error) {
	return runEmailPoolWithRetry("return_to_pool", func() (*emailPoolRecord, error) {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = tx.Rollback()
		}()

		record, err := fetchEmailPoolRecord(ctx, tx, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", accountID)
		if err != nil || record == nil {
			return record, err
		}
		if record.Status != emailPoolStatusLeased {
			return nil, fmt.Errorf("当前状态为 %s，不能归还", record.Status)
		}
		if record.LeaseToken == nil || *record.LeaseToken != leaseToken {
			return nil, errors.New("lease_token 不匹配，不能归还")
		}

		now := utcNowString()
		if _, err := tx.ExecContext(
			ctx,
			"UPDATE email_accounts SET status = ?, lease_token = NULL, leased_at = NULL, updated_at = ? WHERE id = ?",
			emailPoolStatusAvailable,
			now,
			accountID,
		); err != nil {
			return nil, err
		}

		updated, err := fetchEmailPoolRecord(ctx, tx, "SELECT "+emailPoolSelectColumns+" FROM email_accounts WHERE id = ?", accountID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// reclaimExpiredLeases 回收超时未确认的租约。
// Why: 调用方崩溃后邮箱可能长期卡在 leased 状态，自动回收是避免账号池被占死的关键兜底机制。
func (store *emailPoolStore) reclaimExpiredLeases(ctx context.Context, leaseTimeoutSeconds int) (emailPoolReclaimResult, error) {
	return runEmailPoolWithRetry("reclaim_expired_leases", func() (emailPoolReclaimResult, error) {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return emailPoolReclaimResult{}, err
		}
		defer func() {
			_ = tx.Rollback()
		}()

		now := time.Now().UTC()
		expireBefore := now.Add(-time.Duration(leaseTimeoutSeconds) * time.Second)
		rows, err := tx.QueryContext(
			ctx,
			"SELECT id, leased_at FROM email_accounts WHERE status = ? AND leased_at IS NOT NULL ORDER BY id ASC",
			emailPoolStatusLeased,
		)
		if err != nil {
			return emailPoolReclaimResult{}, err
		}
		defer func() {
			_ = rows.Close()
		}()

		result := emailPoolReclaimResult{
			ReclaimedIDs:        []int{},
			CheckedAt:           now.Format(time.RFC3339Nano),
			LeaseTimeoutSeconds: leaseTimeoutSeconds,
		}
		for rows.Next() {
			var accountID int
			var leasedAtRaw string
			if err := rows.Scan(&accountID, &leasedAtRaw); err != nil {
				return emailPoolReclaimResult{}, err
			}

			leasedAt, err := time.Parse(time.RFC3339Nano, leasedAtRaw)
			if err != nil || leasedAt.After(expireBefore) {
				continue
			}

			updateResult, err := tx.ExecContext(
				ctx,
				"UPDATE email_accounts SET status = ?, lease_token = NULL, leased_at = NULL, updated_at = ? WHERE id = ? AND status = ?",
				emailPoolStatusAvailable,
				result.CheckedAt,
				accountID,
				emailPoolStatusLeased,
			)
			if err != nil {
				return emailPoolReclaimResult{}, err
			}

			affected, err := updateResult.RowsAffected()
			if err != nil {
				return emailPoolReclaimResult{}, err
			}
			if affected > 0 {
				result.ReclaimedIDs = append(result.ReclaimedIDs, accountID)
			}
		}
		if err := rows.Err(); err != nil {
			return emailPoolReclaimResult{}, err
		}
		result.ReclaimedCount = len(result.ReclaimedIDs)

		if err := tx.Commit(); err != nil {
			return emailPoolReclaimResult{}, err
		}
		return result, nil
	})
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
func newEmailPoolService(store *emailPoolStore, emailsFile string, mailClient *upstreamMailClient, leaseTimeoutSeconds int) *emailPoolService {
	return &emailPoolService{
		store:               store,
		emailsFile:          emailsFile,
		mailClient:          mailClient,
		leaseTimeoutSeconds: leaseTimeoutSeconds,
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
// Why: 账号池文件可能被外部脚本持续追加，因此服务端必须在处理请求前懒同步，避免长驻进程拿到过期数据。
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
	service.fileSignature = currentSignature
	service.lastSyncResult = result
	return result, nil
}

// ensureReclaimed 以最小检查间隔执行租约回收。
// Why: 高频请求下每次都扫描 leased 记录会放大数据库压力，因此这里显式做节流。
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
		service.respondUnexpected(writer, err)
		return
	}

	switch {
	case path == "/api/email-pool/stats":
		stats, err := service.store.getStats(request.Context())
		if err != nil {
			service.respondUnexpected(writer, err)
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
		service.respondError(writer, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	if path != "/api/email-pool/sync" {
		if err := service.prepareRequestData(request.Context()); err != nil {
			service.respondUnexpected(writer, err)
			return
		}
	}

	switch {
	case path == "/api/email-pool/sync":
		syncResult, err := service.ensureSynced(request.Context(), true)
		if err != nil {
			service.respondUnexpected(writer, err)
			return
		}
		reclaimResult, err := service.ensureReclaimed(request.Context(), true)
		if err != nil {
			service.respondUnexpected(writer, err)
			return
		}
		service.respondEnvelope(writer, http.StatusOK, emailPoolSyncEnvelopeData{
			Sync:    syncResult,
			Reclaim: reclaimResult,
		})
	case path == "/api/email-pool/lease":
		record, err := service.store.leaseOne(request.Context())
		if err != nil {
			service.respondUnexpected(writer, err)
			return
		}
		if record == nil {
			service.respondError(writer, http.StatusNotFound, "当前没有可用邮箱账号")
			return
		}
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case path == "/api/email-pool/accounts/by-email/latest":
		email := strings.TrimSpace(fmt.Sprintf("%v", payload["email"]))
		mailbox := fmt.Sprintf("%v", payload["mailbox"])
		service.handleLatestMailByEmail(writer, request, email, mailbox)
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/mark-used"):
		accountID, err := extractEmailPoolAccountID(path, "/mark-used")
		if err != nil {
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		leaseToken := strings.TrimSpace(fmt.Sprintf("%v", payload["lease_token"]))
		if leaseToken == "" {
			service.respondError(writer, http.StatusBadRequest, "lease_token 必填")
			return
		}
		record, err := service.store.markUsed(request.Context(), accountID, leaseToken)
		if err != nil {
			if isLeaseConflict(err) {
				service.respondError(writer, http.StatusConflict, err.Error())
				return
			}
			service.respondUnexpected(writer, err)
			return
		}
		if record == nil {
			service.respondError(writer, http.StatusNotFound, "账号不存在")
			return
		}
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/return"):
		accountID, err := extractEmailPoolAccountID(path, "/return")
		if err != nil {
			service.respondError(writer, http.StatusBadRequest, "账号 ID 非法")
			return
		}
		leaseToken := strings.TrimSpace(fmt.Sprintf("%v", payload["lease_token"]))
		if leaseToken == "" {
			service.respondError(writer, http.StatusBadRequest, "lease_token 必填")
			return
		}
		record, err := service.store.returnToPool(request.Context(), accountID, leaseToken)
		if err != nil {
			if isLeaseConflict(err) {
				service.respondError(writer, http.StatusConflict, err.Error())
				return
			}
			service.respondUnexpected(writer, err)
			return
		}
		if record == nil {
			service.respondError(writer, http.StatusNotFound, "账号不存在")
			return
		}
		service.respondEnvelope(writer, http.StatusOK, record.publicAccount())
	case strings.HasPrefix(path, "/api/email-pool/accounts/") && strings.HasSuffix(path, "/latest"):
		accountID, err := extractEmailPoolAccountID(path, "/latest")
		if err != nil {
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
		service.respondError(writer, http.StatusBadRequest, err.Error())
		return
	}

	record, err := service.store.getAccount(request.Context(), accountID)
	if err != nil {
		service.respondUnexpected(writer, err)
		return
	}
	if record == nil {
		service.respondError(writer, http.StatusNotFound, "账号不存在")
		return
	}

	latestMail, err := service.mailClient.fetchLatestMail(request.Context(), *record, normalizedMailbox)
	if err != nil {
		service.respondError(writer, http.StatusBadGateway, err.Error())
		return
	}
	service.respondEnvelope(writer, http.StatusOK, map[string]any{
		"account":     record.publicAccount(),
		"mailbox":     normalizedMailbox,
		"latest_mail": latestMail,
	})
}

func (service *emailPoolService) handleLatestMailByEmail(writer http.ResponseWriter, request *http.Request, email string, mailbox string) {
	if email == "" {
		service.respondError(writer, http.StatusBadRequest, "email 必填")
		return
	}

	normalizedMailbox, err := normalizeEmailPoolMailbox(mailbox)
	if err != nil {
		service.respondError(writer, http.StatusBadRequest, err.Error())
		return
	}

	record, err := service.store.getAccountByEmail(request.Context(), email)
	if err != nil {
		service.respondUnexpected(writer, err)
		return
	}
	if record == nil {
		service.respondError(writer, http.StatusNotFound, "邮箱账号不存在")
		return
	}

	latestMail, err := service.mailClient.fetchLatestMail(request.Context(), *record, normalizedMailbox)
	if err != nil {
		service.respondError(writer, http.StatusBadGateway, err.Error())
		return
	}
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

func (service *emailPoolService) respondUnexpected(writer http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	message := "服务内部错误"
	if isSQLiteBusyError(err) {
		statusCode = http.StatusServiceUnavailable
		message = err.Error()
	}
	service.respondError(writer, statusCode, message)
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
