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

// tryLockFileExclusivePlatform 在 Unix 上尝试非阻塞获取 flock。
// Why: 只有先尝试非阻塞加锁，外层才能在等待期间持续打印“还在等锁”而不是直接卡在系统调用里。
func tryLockFileExclusivePlatform(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return false, err
}

// unlockFilePlatform 在 Unix 系平台上释放 flock 锁。
func unlockFilePlatform(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
