package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// authorizationAttemptResult 记录单个账号授权尝试的结果。
// Why: 批量授权和 pipeline 模式都需要聚合成功/失败统计，因此这里把 worker 返回值统一成稳定结构。
type authorizationAttemptResult struct {
	Email        string
	OAuthStatus  string
	AuthFilePath string
	Err          error
}

// runAuthorizeFromAccounts 从 accounts.txt 中提取待授权账号并批量执行授权。
func runAuthorizeFromAccounts(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore) error {
	pending, err := store.listPendingAuthorization()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		logger.Printf("accounts.txt 中没有待授权账号: %s", cfg.accountsFile)
		return nil
	}

	logger.Printf("发现 %d 个待授权账号，开始批量授权", len(pending))
	return runAuthorizeRecords(parent, cfg, mailClient, logger, store, pending, cfg.workers)
}

// runPipeline 同时运行注册 worker 和授权 worker。
// Why: 注册成功后立即把账号放入统一状态文件，再交给独立授权 worker 消费，可以避免串行等待放大整体耗时。
func runPipeline(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore) error {
	authorizeJobs := make(chan accountRecord, maxInt(cfg.authorizeWorkers*2, 1))
	authorizeResults := make(chan authorizationAttemptResult, maxInt(cfg.authorizeWorkers*2, 1))

	var authorizeWG sync.WaitGroup
	startAuthorizeWorkers(parent, cfg, mailClient, logger, store, cfg.authorizeWorkers, authorizeJobs, authorizeResults, &authorizeWG)

	registerErr := runRegister(parent, cfg, mailClient, logger, store, authorizeJobs)
	close(authorizeJobs)
	authorizeWG.Wait()
	close(authorizeResults)

	success := 0
	fail := 0
	var firstErr error
	for result := range authorizeResults {
		if isOAuthSuccessful(result.OAuthStatus) {
			success++
			continue
		}
		fail++
		logger.Printf("pipeline 授权失败账号=%s status=%s err=%v", result.Email, result.OAuthStatus, result.Err)
		if firstErr == nil && result.Err != nil {
			firstErr = result.Err
		}
	}

	logger.Printf("pipeline 授权阶段结束: success=%d fail=%d", success, fail)
	if registerErr != nil && firstErr != nil {
		return fmt.Errorf("注册阶段失败: %w；授权阶段失败: %v", registerErr, firstErr)
	}
	if registerErr != nil {
		return registerErr
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func runAuthorizeRecords(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, records []accountRecord, workerCount int) error {
	jobs := make(chan accountRecord, len(records))
	results := make(chan authorizationAttemptResult, len(records))

	var workers sync.WaitGroup
	startAuthorizeWorkers(parent, cfg, mailClient, logger, store, workerCount, jobs, results, &workers)

	for _, record := range records {
		jobs <- record
	}
	close(jobs)
	workers.Wait()
	close(results)

	success := 0
	fail := 0
	var firstErr error
	for result := range results {
		if isOAuthSuccessful(result.OAuthStatus) {
			success++
			continue
		}
		fail++
		logger.Printf("授权失败账号=%s status=%s err=%v", result.Email, result.OAuthStatus, result.Err)
		if firstErr == nil && result.Err != nil {
			firstErr = result.Err
		}
	}

	logger.Printf("授权任务结束: success=%d fail=%d", success, fail)
	if success == 0 && fail > 0 && firstErr != nil {
		return fmt.Errorf("全部授权失败: %w", firstErr)
	}
	return nil
}

func startAuthorizeWorkers(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, workerCount int, jobs <-chan accountRecord, results chan<- authorizationAttemptResult, wg *sync.WaitGroup) {
	if workerCount <= 0 {
		workerCount = 1
	}

	for workerID := 1; workerID <= workerCount; workerID++ {
		workerID := workerID
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range jobs {
				results <- runAuthorizeAttempt(parent, cfg, mailClient, logger, store, workerID, record)
			}
		}()
	}
}

// runAuthorizeAttempt 负责单个账号的一次 OAuth 授权尝试，并把结果回写到 accounts.txt。
func runAuthorizeAttempt(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, workerID int, record accountRecord) authorizationAttemptResult {
	attemptCtx, cancel := context.WithTimeout(parent, cfg.overallTimeout)
	defer cancel()

	prefix := fmt.Sprintf("[auth-%d][%s]", workerID, record.Email)
	logger.Printf("%s 开始授权", prefix)

	result, err := loginWithProtocol(attemptCtx, cfg, loginAccount{
		email:    record.Email,
		password: record.Password,
	}, mailClient, logger)
	if err != nil {
		reason := summarizeFlowReason(err)
		status := "oauth=fail:" + reason
		if _, writeErr := store.upsertOAuthResult(record.Email, record.Password, status, time.Now(), ""); writeErr != nil {
			return authorizationAttemptResult{
				Email:       record.Email,
				OAuthStatus: status,
				Err:         fmt.Errorf("授权失败: %v；回写 accounts 失败: %w", err, writeErr),
			}
		}
		logger.Printf("%s 授权失败: %v", prefix, err)
		return authorizationAttemptResult{
			Email:       record.Email,
			OAuthStatus: status,
			Err:         err,
		}
	}

	if _, err := store.upsertOAuthResult(record.Email, record.Password, "oauth=ok", time.Now(), result.AuthFilePath); err != nil {
		return authorizationAttemptResult{
			Email:       record.Email,
			OAuthStatus: "oauth=ok",
			Err:         fmt.Errorf("授权成功但回写 accounts 失败: %w", err),
		}
	}

	logger.Printf("%s 授权成功: %s", prefix, result.AuthFilePath)
	return authorizationAttemptResult{
		Email:        record.Email,
		OAuthStatus:  "oauth=ok",
		AuthFilePath: result.AuthFilePath,
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
