//go:build unix

package utils

import (
	"errors"
	"os"
	"syscall"
)

// flock 的锁归属于「打开文件描述」而不是进程，进程退出时内核自动释放，
// 所以被 kill -9 也不会留下死锁。
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrInstanceLocked
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
