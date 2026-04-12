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
