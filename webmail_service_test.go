package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseEmailPoolLine 验证 emails.txt 行解析规则与附加字段保留逻辑。
func TestParseEmailPoolLine(t *testing.T) {
	t.Parallel()

	record, err := parseEmailPoolLine("demo@example.com----pass----client----refresh----extra1----extra2")
	if err != nil {
		t.Fatalf("expected parse success, got error: %v", err)
	}
	if record == nil {
		t.Fatal("expected record, got nil")
	}
	if record.Email != "demo@example.com" || record.Password != "pass" || record.ClientID != "client" || record.RefreshToken != "refresh" {
		t.Fatalf("unexpected parsed record: %#v", record)
	}
	if len(record.ExtraFields) != 2 || record.ExtraFields[0] != "extra1" || record.ExtraFields[1] != "extra2" {
		t.Fatalf("expected extra fields to be preserved, got %#v", record.ExtraFields)
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
	dbPath := filepath.Join(tempDir, "email_pool.sqlite3")
	writeEmailPoolFile(t, emailsFile,
		"alpha@example.com----pass-a----client-a----refresh-a",
		"beta@example.com----pass-b----client-b----refresh-b",
	)

	store := newTestEmailPoolStore(t, dbPath)
	ctx := context.Background()

	firstSync, err := store.syncFromFile(ctx, emailsFile)
	if err != nil {
		t.Fatalf("expected first sync success, got error: %v", err)
	}
	if firstSync.Inserted != 2 || firstSync.Updated != 0 || firstSync.Skipped != 0 {
		t.Fatalf("unexpected first sync result: %#v", firstSync)
	}

	secondSync, err := store.syncFromFile(ctx, emailsFile)
	if err != nil {
		t.Fatalf("expected second sync success, got error: %v", err)
	}
	if secondSync.Skipped != 2 {
		t.Fatalf("expected second sync to skip 2 records, got %#v", secondSync)
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
	if stats.Total != 3 || stats.Available != 3 || stats.Leased != 0 || stats.Used != 0 {
		t.Fatalf("unexpected stats: %#v", stats)
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

	stats, err = store.getStats(ctx)
	if err != nil {
		t.Fatalf("expected stats success after state transitions, got error: %v", err)
	}
	if stats.Total != 3 || stats.Available != 2 || stats.Used != 1 {
		t.Fatalf("unexpected final stats: %#v", stats)
	}

	alphaRecord, err := store.getAccountByEmail(ctx, "alpha@example.com")
	if err != nil {
		t.Fatalf("expected lookup success, got error: %v", err)
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
	dbPath := filepath.Join(tempDir, "email_pool.sqlite3")
	writeEmailPoolFile(t, emailsFile, "reclaim@example.com----pass----client----refresh")

	store := newTestEmailPoolStore(t, dbPath)
	ctx := context.Background()
	if _, err := store.syncFromFile(ctx, emailsFile); err != nil {
		t.Fatalf("expected sync success, got error: %v", err)
	}

	leased, err := store.leaseOne(ctx)
	if err != nil {
		t.Fatalf("expected lease success, got error: %v", err)
	}
	expiredAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(ctx, "UPDATE email_accounts SET leased_at = ? WHERE id = ?", expiredAt, leased.ID); err != nil {
		t.Fatalf("expected test fixture update success, got error: %v", err)
	}

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

// TestEmailPoolServiceHTTP 验证 HTTP 路由兼容性与 envelope 结构。
func TestEmailPoolServiceHTTP(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	dbPath := filepath.Join(tempDir, "email_pool.sqlite3")
	writeEmailPoolFile(t, emailsFile, "http@example.com----pass----client-http----refresh-http")

	store := newTestEmailPoolStore(t, dbPath)
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

	service := newEmailPoolService(store, emailsFile, newUpstreamMailClient(upstreamServer.URL, 2*time.Second), 600)
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
}

// TestRunWebMailServerSyncOnly 覆盖 CLI `webmail` 模式下的 sync-only 入口。
func TestRunWebMailServerSyncOnly(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	emailsFile := filepath.Join(tempDir, "emails.txt")
	dbPath := filepath.Join(tempDir, "email_pool.sqlite3")
	writeEmailPoolFile(t, emailsFile, "sync@example.com----pass----client----refresh")

	usedTUI, err := run(context.Background(), []string{
		"-mode", "webmail",
		"-web-mail-sync-only",
		"-web-mail-db", dbPath,
		"-web-mail-emails-file", emailsFile,
		"-mail-api-base", "http://127.0.0.1:65535",
	})
	if err != nil {
		t.Fatalf("expected sync-only run success, got error: %v", err)
	}
	if usedTUI {
		t.Fatal("expected webmail mode to skip TUI")
	}

	store := newTestEmailPoolStore(t, dbPath)
	stats, err := store.getStats(context.Background())
	if err != nil {
		t.Fatalf("expected stats success after sync-only run, got error: %v", err)
	}
	if stats.Total != 1 || stats.Available != 1 {
		t.Fatalf("unexpected sync-only stats: %#v", stats)
	}
}

func newTestEmailPoolStore(t *testing.T, dbPath string) *emailPoolStore {
	t.Helper()

	store, err := newEmailPoolStore(dbPath)
	if err != nil {
		t.Fatalf("expected store init success, got error: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func writeEmailPoolFile(t *testing.T, path string, lines ...string) {
	t.Helper()

	content := []byte(stringsJoin(lines, "\n") + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("expected emails file write success, got error: %v", err)
	}
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
