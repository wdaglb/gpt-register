package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	lockWaitRetryInterval    = 100 * time.Millisecond
	lockWaitProgressInterval = 1 * time.Second
)

// lockFileExclusive 为当前进程获取一个跨平台独占文件锁。
// Why: accounts.txt 和 emails.txt 都会被多线程、甚至多进程并发更新；
// 这里把平台差异收口到统一包装函数，避免业务层直接依赖某个操作系统专有 API。
func lockFileExclusive(file *os.File) error {
	if file == nil {
		return fmt.Errorf("lock file is nil")
	}
	return lockFileExclusivePlatform(file)
}

// unlockFile 释放 lockFileExclusive 获取的文件锁。
// Why: 调用方只需要表达“释放锁”这个意图，不需要知道不同平台的底层解锁实现细节。
func unlockFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("lock file is nil")
	}
	return unlockFilePlatform(file)
}

// lockFileExclusiveWithProgress 以可观测方式等待文件锁，便于定位“卡住”是否发生在 flock/LockFileEx。
// Why: 文本数据库大量依赖文件锁串行化；如果这里只是静默阻塞，外层线程会看起来像完全不动。
func lockFileExclusiveWithProgress(file *os.File, onProgress func(time.Duration)) error {
	if file == nil {
		return fmt.Errorf("lock file is nil")
	}
	if acquired, err := tryLockFileExclusivePlatform(file); err != nil {
		return err
	} else if acquired {
		return nil
	}

	return waitWithProgress(func() (bool, error) {
		return tryLockFileExclusivePlatform(file)
	}, onProgress)
}

// lockMutexWithProgress 以可观测方式等待进程内互斥锁。
// Why: 同一进程内高并发更新 accounts/emails 时，先卡住的往往不是文件锁，而是最外层 mutex。
func lockMutexWithProgress(mu *sync.Mutex, onProgress func(time.Duration)) {
	if mu == nil {
		return
	}
	if mu.TryLock() {
		return
	}

	_ = waitWithProgress(func() (bool, error) {
		return mu.TryLock(), nil
	}, onProgress)
}

// waitWithProgress 在资源尚未获取到时按固定间隔回调进度。
// Why: 各类“等待锁”“等待清理”本质都是同一种心跳需求，统一在这里实现可以避免散落重复 ticker 逻辑。
func waitWithProgress(tryAcquire func() (bool, error), onProgress func(time.Duration)) error {
	startedAt := time.Now()
	lastReported := time.Duration(0)

	for {
		acquired, err := tryAcquire()
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}

		maybeReportWaitProgress(startedAt, &lastReported, lockWaitProgressInterval, onProgress)
		time.Sleep(lockWaitRetryInterval)
	}
}
