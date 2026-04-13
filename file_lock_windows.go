//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

const windowsWholeFileLockLength = ^uint32(0)

// lockFileExclusivePlatform 在 Windows 上使用 LockFileEx 模拟“整文件独占锁”。
// Why: Windows 没有 syscall.Flock；GitHub Actions 的 windows 构建要通过，
// 同时运行期仍需要和 Unix 版本一样，防止多个进程同时覆盖同一份文本数据库。
func lockFileExclusivePlatform(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		windowsWholeFileLockLength,
		windowsWholeFileLockLength,
		overlapped,
	)
}

// tryLockFileExclusivePlatform 在 Windows 上尝试非阻塞获取整文件独占锁。
// Why: system 调用直接阻塞时外层无法输出等待心跳，因此这里显式使用 FAIL_IMMEDIATELY 先做轮询式尝试。
func tryLockFileExclusivePlatform(file *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		windowsWholeFileLockLength,
		windowsWholeFileLockLength,
		overlapped,
	)
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return false, err
}

// unlockFilePlatform 释放 Windows 上的整文件独占锁。
func unlockFilePlatform(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		windowsWholeFileLockLength,
		windowsWholeFileLockLength,
		overlapped,
	)
}
