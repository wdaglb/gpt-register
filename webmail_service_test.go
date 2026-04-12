package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseEmailPoolLine 验证 emails.txt 行解析规则与托管状态字段识别逻辑。
func TestParseEmailPoolLine(t *testing.T) {
	t.Parallel()

	record, err := parseEmailPoolLine("demo@example.com----pass----client----refresh----extra1----id:7----created_at:2026-04-12T11:00:00Z----lease_token:token-1----leased_at:2026-04-12T12:00:00Z----used_at:2026-04-12T13:00:00Z----status:leased")
	if err != nil {
		t.Fatalf("expected parse success, got error: %v", err)
	}
	if record == nil {
		t.Fatal("expected record, got nil")
	}
	if record.Email != "demo@example.com" || record.Password != "pass" || record.ClientID != "client" || record.RefreshToken != "refresh" {
		t.Fatalf("unexpected parsed record: %#v", record)
	}
	if len(record.ExtraFields) != 1 || record.ExtraFields[0] != "extra1" {
		t.Fatalf("expected extra fields to be preserved, got %#v", record.ExtraFields)
	}
	if !record.HasStatus || record.Status != emailPoolStatusLeased {
		t.Fatalf("expected leased status, got %#v", record)
	}
	if !record.HasLeaseToken || record.LeaseToken == nil || *record.LeaseToken != "token-1" {
		t.Fatalf("expected lease token metadata, got %#v", record)
	}
	if !record.HasLeasedAt || record.LeasedAt == nil || *record.LeasedAt != "2026-04-12T12:00:00Z" {
		t.Fatalf("expected leased_at metadata, got %#v", record)
	}
	if !record.HasUsedAt || record.UsedAt == nil || *record.UsedAt != "2026-04-12T13:00:00Z" {
		t.Fatalf("expected used_at metadata, got %#v", record)
	}

	emptyRecord, err := parseEmailPoolLine("   ")
	if err != nil {
		t.Fatalf("expected empty line success, got error: %v", err)
	}
	if emptyRecord != nil {
		t.Fatalf("expected nil record for empty line, got %#v", emptyRecord)
	}
}

// TestEmailPoolStoreLifecycle 覆盖同步、租约、归还和标记已使用的主状态流转。
func TestEmailPoolStoreLifecycle(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile,
		"alpha@example.com----pass-a----client-a----refresh-a",
		"beta@example.com----pass-b----client-b----refresh-b",
	)

	store := newTestEmailPoolStore(t, emailsFile)
	ctx := context.Background()

	firstSync, err := store.syncFromFile(ctx, emailsFile)
	if err != nil {
		t.Fatalf("expected first sync success, got error: %v", err)
	}
	if firstSync.Inserted != 2 || firstSync.Updated != 0 || firstSync.Skipped != 0 {
		t.Fatalf("unexpected first sync result: %#v", firstSync)
	}

	rawAfterFirstSync := readEmailPoolFile(t, emailsFile)
	if strings.Contains(rawAfterFirstSync, "status:available") || strings.Contains(rawAfterFirstSync, "id:") || strings.Contains(rawAfterFirstSync, "created_at:") || strings.Contains(rawAfterFirstSync, "updated_at:") || strings.Contains(rawAfterFirstSync, "source_updated_at:") {
		t.Fatalf("expected default available state to stay implicit, got %s", rawAfterFirstSync)
	}

	secondSync, err := store.syncFromFile(ctx, emailsFile)
	if err != nil {
		t.Fatalf("expected second sync success, got error: %v", err)
	}
	if secondSync.Skipped != 2 {
		t.Fatalf("expected second sync to skip 2 records, got %#v", secondSync)
	}

	leased, err := store.leaseOne(ctx)
	if err != nil {
		t.Fatalf("expected lease success, got error: %v", err)
	}
	if leased == nil || leased.Status != emailPoolStatusLeased || leased.LeaseToken == nil {
		t.Fatalf("unexpected leased record: %#v", leased)
	}

	returned, err := store.returnToPool(ctx, leased.ID, *leased.LeaseToken)
	if err != nil {
		t.Fatalf("expected return success, got error: %v", err)
	}
	if returned.Status != emailPoolStatusAvailable || returned.LeaseToken != nil {
		t.Fatalf("unexpected returned record: %#v", returned)
	}

	releasedAgain, err := store.leaseOne(ctx)
	if err != nil {
		t.Fatalf("expected second lease success, got error: %v", err)
	}
	usedRecord, err := store.markUsed(ctx, releasedAgain.ID, *releasedAgain.LeaseToken)
	if err != nil {
		t.Fatalf("expected mark used success, got error: %v", err)
	}
	if usedRecord.Status != emailPoolStatusUsed || usedRecord.LeaseToken != nil {
		t.Fatalf("unexpected used record: %#v", usedRecord)
	}

	writeEmailPoolFile(t, emailsFile,
		"alpha@example.com----pass-a-updated----client-a----refresh-a",
		"beta@example.com----pass-b----client-b----refresh-b",
		"gamma@example.com----pass-c----client-c----refresh-c",
	)
	thirdSync, err := store.syncFromFile(ctx, emailsFile)
	if err != nil {
		t.Fatalf("expected third sync success, got error: %v", err)
	}
	if thirdSync.Inserted != 1 || thirdSync.Updated != 1 || thirdSync.Skipped != 1 {
		t.Fatalf("unexpected third sync result: %#v", thirdSync)
	}

	stats, err := store.getStats(ctx)
	if err != nil {
		t.Fatalf("expected stats success, got error: %v", err)
	}
	if stats.Total != 3 || stats.Available != 2 || stats.Leased != 0 || stats.Used != 1 {
		t.Fatalf("unexpected stats: %#v", stats)
	}

	usedEmail := usedRecord.Email
	usedPersistedRecord, err := store.getAccountByEmail(ctx, usedEmail)
	if err != nil {
		t.Fatalf("expected lookup success, got error: %v", err)
	}
	if usedPersistedRecord == nil || usedPersistedRecord.Status != emailPoolStatusUsed {
		t.Fatalf("expected used status to be preserved for %s, got %#v", usedEmail, usedPersistedRecord)
	}
	rawAfterUsed := readEmailPoolFile(t, emailsFile)
	if !strings.Contains(rawAfterUsed, "status:used") {
		t.Fatalf("expected non-default used state to be persisted, got %s", rawAfterUsed)
	}

	alphaRecord, err := store.getAccountByEmail(ctx, "alpha@example.com")
	if err != nil {
		t.Fatalf("expected alpha lookup success, got error: %v", err)
	}
	if alphaRecord == nil || alphaRecord.Password != "pass-a-updated" {
		t.Fatalf("expected updated password to be persisted, got %#v", alphaRecord)
	}
}

// TestEmailPoolStoreReclaimExpiredLeases 验证租约超时回收不会遗漏已卡死的 leased 记录。
func TestEmailPoolStoreReclaimExpiredLeases(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile, "reclaim@example.com----pass----client----refresh")

	store := newTestEmailPoolStore(t, emailsFile)
	ctx := context.Background()
	if _, err := store.syncFromFile(ctx, emailsFile); err != nil {
		t.Fatalf("expected sync success, got error: %v", err)
	}

	leased, err := store.leaseOne(ctx)
	if err != nil {
		t.Fatalf("expected lease success, got error: %v", err)
	}
	expiredAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	updateEmailPoolLeasedAtForTest(t, store, leased.ID, expiredAt)

	reclaimResult, err := store.reclaimExpiredLeases(ctx, 60)
	if err != nil {
		t.Fatalf("expected reclaim success, got error: %v", err)
	}
	if reclaimResult.ReclaimedCount != 1 || len(reclaimResult.ReclaimedIDs) != 1 || reclaimResult.ReclaimedIDs[0] != leased.ID {
		t.Fatalf("unexpected reclaim result: %#v", reclaimResult)
	}

	record, err := store.getAccount(ctx, leased.ID)
	if err != nil {
		t.Fatalf("expected account lookup success, got error: %v", err)
	}
	if record.Status != emailPoolStatusAvailable || record.LeaseToken != nil {
		t.Fatalf("expected reclaimed account to become available, got %#v", record)
	}
}

// TestEmailPoolStoreConcurrentLease 验证 txt 持久化下并发租号不会重复分配同一条记录。
func TestEmailPoolStoreConcurrentLease(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile,
		"a@example.com----pass----client-a----refresh-a",
		"b@example.com----pass----client-b----refresh-b",
		"c@example.com----pass----client-c----refresh-c",
	)

	store := newTestEmailPoolStore(t, emailsFile)
	ctx := context.Background()
	if _, err := store.syncFromFile(ctx, emailsFile); err != nil {
		t.Fatalf("expected sync success, got error: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan *emailPoolRecord, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, err := store.leaseOne(ctx)
			if err != nil {
				t.Errorf("expected lease success, got error: %v", err)
				return
			}
			results <- record
		}()
	}
	wg.Wait()
	close(results)

	seenIDs := map[int]struct{}{}
	leasedCount := 0
	for record := range results {
		if record == nil {
			continue
		}
		leasedCount++
		if _, exists := seenIDs[record.ID]; exists {
			t.Fatalf("duplicate leased id detected: %d", record.ID)
		}
		seenIDs[record.ID] = struct{}{}
	}
	if leasedCount != 3 {
		t.Fatalf("expected exactly 3 leased records, got %d", leasedCount)
	}
}

// TestEmailPoolServiceHTTP 验证 HTTP 路由兼容性与 envelope 结构。
func TestEmailPoolServiceHTTP(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile, "http@example.com----pass----client-http----refresh-http")

	store := newTestEmailPoolStore(t, emailsFile)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload := map[string]any{
			"data": map[string]any{
				"subject":  "Your code is 654321",
				"textBody": "654321",
				"date":     "2026-04-12T12:00:00Z",
				"email":    request.URL.Query().Get("email"),
				"mailbox":  request.URL.Query().Get("mailbox"),
			},
		}
		_ = json.NewEncoder(writer).Encode(payload)
	}))
	defer upstreamServer.Close()

	var logBuffer bytes.Buffer
	service := newEmailPoolService(
		store,
		emailsFile,
		newUpstreamMailClient(upstreamServer.URL, 2*time.Second),
		600,
		log.New(&logBuffer, "", 0),
	)
	server := httptest.NewServer(service)
	defer server.Close()

	healthStatus, healthBody := httpJSONRequest(t, http.MethodGet, server.URL+"/health", nil)
	if healthStatus != http.StatusOK || healthBody["ok"] != true || healthBody["service"] != "email-pool" {
		t.Fatalf("unexpected health response: status=%d body=%#v", healthStatus, healthBody)
	}

	statsStatus, statsBody := httpJSONRequest(t, http.MethodGet, server.URL+"/api/email-pool/stats", nil)
	if statsStatus != http.StatusOK || statsBody["ok"] != true {
		t.Fatalf("unexpected stats response: status=%d body=%#v", statsStatus, statsBody)
	}
	statsData := statsBody["data"].(map[string]any)
	if int(statsData["total"].(float64)) != 1 || int(statsData["available"].(float64)) != 1 {
		t.Fatalf("unexpected stats data: %#v", statsData)
	}

	leaseStatus, leaseBody := httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/lease", map[string]any{})
	if leaseStatus != http.StatusOK || leaseBody["ok"] != true {
		t.Fatalf("unexpected lease response: status=%d body=%#v", leaseStatus, leaseBody)
	}
	leasedAccount := leaseBody["data"].(map[string]any)
	leaseID := int(leasedAccount["id"].(float64))
	leaseToken := leasedAccount["lease_token"].(string)
	if leaseID <= 0 {
		t.Fatalf("expected generated stable id, got %#v", leasedAccount)
	}

	latestStatus, latestBody := httpJSONRequest(t, http.MethodGet, server.URL+"/api/email-pool/accounts/"+strconvString(leaseID)+"/latest?mailbox=Junk", nil)
	if latestStatus != http.StatusOK || latestBody["ok"] != true {
		t.Fatalf("unexpected latest-by-id response: status=%d body=%#v", latestStatus, latestBody)
	}
	latestData := latestBody["data"].(map[string]any)
	if latestData["mailbox"] != "Junk" {
		t.Fatalf("expected Junk mailbox, got %#v", latestData)
	}

	byEmailStatus, byEmailBody := httpJSONRequest(t, http.MethodGet, server.URL+"/api/email-pool/accounts/by-email/latest?email=http@example.com&mailbox=INBOX", nil)
	if byEmailStatus != http.StatusOK || byEmailBody["ok"] != true {
		t.Fatalf("unexpected latest-by-email response: status=%d body=%#v", byEmailStatus, byEmailBody)
	}

	invalidMailboxStatus, invalidMailboxBody := httpJSONRequest(t, http.MethodGet, server.URL+"/api/email-pool/accounts/by-email/latest?email=http@example.com&mailbox=Spam", nil)
	if invalidMailboxStatus != http.StatusBadRequest || invalidMailboxBody["ok"] != false {
		t.Fatalf("expected invalid mailbox error, got status=%d body=%#v", invalidMailboxStatus, invalidMailboxBody)
	}

	returnStatus, returnBody := httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/accounts/"+strconvString(leaseID)+"/return", map[string]any{
		"lease_token": leaseToken,
	})
	if returnStatus != http.StatusOK || returnBody["ok"] != true {
		t.Fatalf("unexpected return response: status=%d body=%#v", returnStatus, returnBody)
	}

	leaseStatus, leaseBody = httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/lease", map[string]any{})
	if leaseStatus != http.StatusOK || leaseBody["ok"] != true {
		t.Fatalf("unexpected second lease response: status=%d body=%#v", leaseStatus, leaseBody)
	}
	leasedAccount = leaseBody["data"].(map[string]any)
	leaseID = int(leasedAccount["id"].(float64))
	leaseToken = leasedAccount["lease_token"].(string)

	markStatus, markBody := httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/accounts/"+strconvString(leaseID)+"/mark-used", map[string]any{
		"lease_token": leaseToken,
	})
	if markStatus != http.StatusOK || markBody["ok"] != true {
		t.Fatalf("unexpected mark-used response: status=%d body=%#v", markStatus, markBody)
	}

	noAvailableStatus, noAvailableBody := httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/lease", map[string]any{})
	if noAvailableStatus != http.StatusNotFound || noAvailableBody["ok"] != false {
		t.Fatalf("expected no-available error, got status=%d body=%#v", noAvailableStatus, noAvailableBody)
	}

	writeEmailPoolFile(t, emailsFile,
		"http@example.com----pass----client-http----refresh-http",
		"new@example.com----pass-new----client-new----refresh-new",
	)
	syncStatus, syncBody := httpJSONRequest(t, http.MethodPost, server.URL+"/api/email-pool/sync", map[string]any{})
	if syncStatus != http.StatusOK || syncBody["ok"] != true {
		t.Fatalf("unexpected sync response: status=%d body=%#v", syncStatus, syncBody)
	}
	syncData := syncBody["data"].(map[string]any)
	if int(syncData["sync"].(map[string]any)["inserted"].(float64)) != 1 {
		t.Fatalf("expected sync to insert 1 new account, got %#v", syncData)
	}

	statsStatus, statsBody = httpJSONRequest(t, http.MethodGet, server.URL+"/api/email-pool/stats", nil)
	if statsStatus != http.StatusOK || statsBody["ok"] != true {
		t.Fatalf("unexpected final stats response: status=%d body=%#v", statsStatus, statsBody)
	}
	statsData = statsBody["data"].(map[string]any)
	if int(statsData["total"].(float64)) != 2 || int(statsData["used"].(float64)) != 1 || int(statsData["available"].(float64)) != 1 {
		t.Fatalf("unexpected final stats data: %#v", statsData)
	}

	logOutput := logBuffer.String()
	expectedLogFragments := []string{
		"租出邮箱 account_id=",
		"获取最新邮件成功: account_id=",
		"归还邮箱成功: account_id=",
		"标记邮箱已使用: account_id=",
		"租号失败: 当前没有可用邮箱账号",
		"web_mail 手动同步完成:",
	}
	for _, fragment := range expectedLogFragments {
		if !strings.Contains(logOutput, fragment) {
			t.Fatalf("expected log output to contain %q, got %s", fragment, logOutput)
		}
	}
}

// TestRunWebMailServerSyncOnly 覆盖 CLI `webmail` 模式下的 sync-only 入口。
func TestRunWebMailServerSyncOnly(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile, "sync@example.com----pass----client----refresh")

	usedTUI, err := run(context.Background(), []string{
		"-mode", "webmail",
		"-web-mail-sync-only",
		"-web-mail-emails-file", emailsFile,
		"-mail-api-base", "http://127.0.0.1:65535",
	})
	if err != nil {
		t.Fatalf("expected sync-only run success, got error: %v", err)
	}
	if usedTUI {
		t.Fatal("expected webmail mode to skip TUI")
	}

	raw := readEmailPoolFile(t, emailsFile)
	if strings.Contains(raw, "status:available") {
		t.Fatalf("expected sync-only run to keep default available state implicit, got %s", raw)
	}

	store := newTestEmailPoolStore(t, emailsFile)
	if _, err := store.syncFromFile(context.Background(), emailsFile); err != nil {
		t.Fatalf("expected store sync success after sync-only run, got error: %v", err)
	}
	stats, err := store.getStats(context.Background())
	if err != nil {
		t.Fatalf("expected stats success after sync-only run, got error: %v", err)
	}
	if stats.Total != 1 || stats.Available != 1 {
		t.Fatalf("unexpected sync-only stats: %#v", stats)
	}
}

// TestEmailPoolServiceEnsureReclaimedLogs 验证超时租约回收时会留下服务端日志，方便定位卡死租约来源。
func TestEmailPoolServiceEnsureReclaimedLogs(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	writeEmailPoolFile(t, emailsFile, "reclaim-log@example.com----pass----client----refresh")

	store := newTestEmailPoolStore(t, emailsFile)
	ctx := context.Background()
	if _, err := store.syncFromFile(ctx, emailsFile); err != nil {
		t.Fatalf("expected sync success, got error: %v", err)
	}

	leased, err := store.leaseOne(ctx)
	if err != nil {
		t.Fatalf("expected lease success, got error: %v", err)
	}
	expiredAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	updateEmailPoolLeasedAtForTest(t, store, leased.ID, expiredAt)

	var logBuffer bytes.Buffer
	service := newEmailPoolService(
		store,
		emailsFile,
		newUpstreamMailClient("http://127.0.0.1:65535", 2*time.Second),
		60,
		log.New(&logBuffer, "", 0),
	)

	reclaimResult, err := service.ensureReclaimed(ctx, true)
	if err != nil {
		t.Fatalf("expected reclaim success, got error: %v", err)
	}
	if reclaimResult.ReclaimedCount != 1 {
		t.Fatalf("expected exactly 1 reclaimed lease, got %#v", reclaimResult)
	}
	if !strings.Contains(logBuffer.String(), "web_mail 已回收超时租约: reclaimed=1") {
		t.Fatalf("expected reclaim log, got %s", logBuffer.String())
	}
}

func newTestEmailPoolStore(t *testing.T, emailsFile string) *emailPoolStore {
	t.Helper()

	store, err := newEmailPoolStore(emailsFile)
	if err != nil {
		t.Fatalf("expected store init success, got error: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func updateEmailPoolLeasedAtForTest(t *testing.T, store *emailPoolStore, accountID int, leasedAt string) {
	t.Helper()

	if err := store.withLockedFile(func() error {
		index := findEmailPoolRecordIndexByID(store.records, accountID)
		if index < 0 {
			return errors.New("test account not found")
		}
		record := &store.records[index]
		record.LeasedAt = &leasedAt
		record.UpdatedAt = leasedAt
		return store.saveRecordsUnlocked()
	}); err != nil {
		t.Fatalf("expected test leased_at update success, got error: %v", err)
	}
}

func writeEmailPoolFile(t *testing.T, path string, lines ...string) {
	t.Helper()

	content := []byte(stringsJoin(lines, "\n") + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("expected emails file write success, got error: %v", err)
	}
}

func readEmailPoolFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected emails file read success, got error: %v", err)
	}
	return string(raw)
}

func httpJSONRequest(t *testing.T, method string, requestURL string, payload map[string]any) (int, map[string]any) {
	t.Helper()

	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("expected payload encode success, got error: %v", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, requestURL, bodyReader)
	if err != nil {
		t.Fatalf("expected request build success, got error: %v", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("expected request success, got error: %v", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	rawBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("expected response read success, got error: %v", err)
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		t.Fatalf("expected JSON response, got error: %v, body=%s", err, string(rawBody))
	}
	return response.StatusCode, decoded
}

func strconvString(value int) string {
	return strconv.FormatInt(int64(value), 10)
}

func stringsJoin(values []string, separator string) string {
	return strings.Join(values, separator)
}
