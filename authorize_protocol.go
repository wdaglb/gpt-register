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

// authorizationSummary 汇总授权阶段的成功/失败计数与首个错误。
// Why: pipeline 需要边消费结果边跑 worker，最后再统一收敛统计，避免结果通道无人消费导致死锁。
type authorizationSummary struct {
	success  int
	fail     int
	firstErr error
}

// runAuthorizeFromAccounts 从 accounts.txt 中提取待授权账号并批量执行授权。
func runAuthorizeFromAccounts(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI) error {
	pending, err := store.listPendingAuthorization()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		logger.Printf("accounts.txt 中没有待授权账号: %s", cfg.accountsFile)
		return nil
	}

	logger.Printf("发现 %d 个待授权账号，开始批量授权", len(pending))
	return runAuthorizeRecords(parent, cfg, mailClient, logger, store, ui, pending, cfg.workers)
}

// runPipeline 在单个 worker 内串行完成“注册成功即授权”的闭环。
// Why: 用户要求注册和授权必须复用同一条指纹/IP 链路，因此不能再把同一账号拆给独立授权 worker。
func runPipeline(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI) error {
	if cfg.authorizeWorkers > 1 {
		logger.Printf("pipeline 模式已切换为单链路串行授权，authorize-workers=%d 当前仅用于兼容旧配置，不再拆分账号内授权线程", cfg.authorizeWorkers)
	}
	return runRegisterUntilTargetSuccess(parent, cfg, mailClient, logger, store, ui, true)
}

func runAuthorizeRecords(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, records []accountRecord, workerCount int) error {
	jobs := make(chan accountRecord, len(records))
	results := make(chan authorizationAttemptResult, len(records))

	var workers sync.WaitGroup
	startAuthorizeWorkers(parent, cfg, mailClient, logger, store, ui, workerCount, jobs, results, &workers)

	for _, record := range records {
		jobs <- record
	}
	close(jobs)
	workers.Wait()
	close(results)
	summary := collectAuthorizationResults(results, logger, "")

	logger.Printf("授权任务结束: success=%d fail=%d", summary.success, summary.fail)
	if summary.success == 0 && summary.fail > 0 && summary.firstErr != nil {
		return fmt.Errorf("全部授权失败: %w", summary.firstErr)
	}
	return nil
}

// startAuthorizeWorkers 把授权 worker 的创建与结果投递集中在一起。
// Why: worker 里除了跑协议，还要同步刷新 TUI 统计，统一放在这里更容易保证 pipeline 和批量授权行为一致。
func startAuthorizeWorkers(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, ui progressUI, workerCount int, jobs <-chan accountRecord, results chan<- authorizationAttemptResult, wg *sync.WaitGroup) {
	if workerCount <= 0 {
		workerCount = 1
	}

	for workerID := 1; workerID <= workerCount; workerID++ {
		workerID := workerID
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range jobs {
				result := runAuthorizeAttempt(parent, cfg, mailClient, logger, store, workerID, record)
				ui.RecordAuthorizeFinish(isAuthorizationSuccessful(result))
				results <- result
			}
		}()
	}
}

// isAuthorizationSuccessful 统一定义“授权成功”的判定条件。
// Why: 只有 OAuth 结果为 ok 且整条 worker 没有返回错误时，TUI 统计和批量汇总才能保持一致。
func isAuthorizationSuccessful(result authorizationAttemptResult) bool {
	return result.Err == nil && isOAuthSuccessful(result.OAuthStatus)
}

// collectAuthorizationResults 统一消费授权结果通道并汇总统计。
// Why: pipeline 模式必须在 worker 运行期间实时 drain 结果通道，否则缓冲打满后会把授权 worker 整体堵住。
func collectAuthorizationResults(results <-chan authorizationAttemptResult, logger *log.Logger, failurePrefix string) authorizationSummary {
	summary := authorizationSummary{}
	for result := range results {
		if isAuthorizationSuccessful(result) {
			summary.success++
			continue
		}
		summary.fail++
		logger.Printf("%s授权失败账号=%s status=%s err=%v", failurePrefix, result.Email, result.OAuthStatus, result.Err)
		if summary.firstErr == nil && result.Err != nil {
			summary.firstErr = result.Err
		}
	}
	return summary
}

// runAuthorizeAttempt 负责单个账号的一次 OAuth 授权尝试，并把结果回写到 accounts.txt。
func runAuthorizeAttempt(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, workerID int, record accountRecord) authorizationAttemptResult {
	attemptCtx, cancel := context.WithTimeout(parent, cfg.overallTimeout)
	defer cancel()

	prefix := fmt.Sprintf("[auth-%d][%s]", workerID, record.Email)
	logger.Printf("%s 开始授权", prefix)
	return authorizeAccountWithClient(attemptCtx, cfg, mailClient, logger, store, record, nil, prefix)
}

// authorizeAccountWithClient 统一执行授权并回写 oauth 状态；当客户端非空时复用现有浏览器链路。
// Why: pipeline 需要注册后立刻沿用同一 protocolClient 继续 OAuth，而独立授权模式仍可以按旧路径新建客户端。
func authorizeAccountWithClient(parent context.Context, cfg config, mailClient *webMailClient, logger *log.Logger, store *accountsStore, record accountRecord, client *protocolClient, prefix string) authorizationAttemptResult {
	var (
		result *protocolLoginResult
		err    error
	)
	if client == nil {
		result, err = loginWithProtocol(parent, cfg, loginAccount{
			email:    record.Email,
			password: record.Password,
		}, mailClient, logger)
	} else {
		result, err = loginWithProtocolClient(parent, cfg, loginAccount{
			email:    record.Email,
			password: record.Password,
		}, mailClient, logger, client)
	}
	if err != nil {
		reason := summarizeFlowReason(err)
		status := "oauth=fail:" + reason
		if store != nil {
			if _, writeErr := store.upsertOAuthResult(record.Email, record.Password, status, time.Now(), ""); writeErr != nil {
				return authorizationAttemptResult{
					Email:       record.Email,
					OAuthStatus: status,
					Err:         fmt.Errorf("授权失败: %v；回写 accounts 失败: %w", err, writeErr),
				}
			}
		}
		logger.Printf("%s 授权失败: %v", prefix, err)
		return authorizationAttemptResult{
			Email:       record.Email,
			OAuthStatus: status,
			Err:         err,
		}
	}

	if store != nil {
		if _, err := store.upsertOAuthResult(record.Email, record.Password, "oauth=ok", time.Now(), result.AuthFilePath); err != nil {
			return authorizationAttemptResult{
				Email:       record.Email,
				OAuthStatus: "oauth=ok",
				Err:         fmt.Errorf("授权成功但回写 accounts 失败: %w", err),
			}
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
