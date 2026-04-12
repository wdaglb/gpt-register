//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockFileExclusivePlatform 在 Unix 系平台上使用 flock 获取独占锁。
// Why: flock 是当前项目原有行为；保留这条实现可以确保 Linux / macOS 的锁语义不变。
func lockFileExclusivePlatform(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
}

// unlockFilePlatform 在 Unix 系平台上释放 flock 锁。
func unlockFilePlatform(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
