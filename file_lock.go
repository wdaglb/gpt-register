package main

import (
	"fmt"
	"os"
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
