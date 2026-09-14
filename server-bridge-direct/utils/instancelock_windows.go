//go:build windows

package utils

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockByteOffset 是加锁的字节偏移。Windows 的 LockFileEx 是强制字节区间锁：
// 被锁住的区间连读都会失败（ERROR_LOCK_VIOLATION）。锁文件开头写的是 pid，
// 要留给运维直接 type/cat 查看，所以把锁打在远超内容长度的位置，只做互斥标记。
const lockByteOffset = 1 << 30

// Windows 上用 LockFileEx + LOCKFILE_FAIL_IMMEDIATELY 拿字节区间独占锁。
// 句柄关闭（含进程异常终止）时锁自动释放，不会留下死锁。
func lockFile(f *os.File) error {
	ol := windows.Overlapped{Offset: lockByteOffset}
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return ErrInstanceLocked
	}
	return err
}

func unlockFile(f *os.File) error {
	ol := windows.Overlapped{Offset: lockByteOffset}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
