package utils

import (
	"errors"
	"fmt"
	"os"
)

// ErrInstanceLocked 表示锁已被另一个进程持有，即已经有一个实例在跑。
var ErrInstanceLocked = errors.New("another instance is already running")

// InstanceLock 是进程级互斥锁，用来保证同一份 bridge.db / 同一份日志文件
// 只有一个进程在写。
//
// 为什么必须有：管理端口和桥端口绑定失败都只会记日志，不会让第二个实例退出，
// 于是两个进程会各自持有一份内存桥集合，各自 dump 整个 bridge.db（temp+rename
// 全量覆盖，后写者赢），也各自跑 lumberjack 轮转（rename 之后另一方的句柄跟着
// 走进归档文件，日志静默丢失）。这两种损坏都不报错，只能靠启动时互斥来排除。
type InstanceLock struct {
	f *os.File
}

// AcquireInstanceLock 以非阻塞方式独占 path。已被其它进程持有时返回
// ErrInstanceLocked，其余 IO 错误原样返回。
//
// 用独立的 .lock 文件而不是直接锁 bridge.db：bridge.db 的落盘走 temp+rename，
// rename 之后旧文件的句柄就不再指向 bridge.db 了，锁会跟着废弃的 inode 走。
func AcquireInstanceLock(path string) (*InstanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	// 写入 pid，纯粹为了运维能查到是谁占着锁。写失败不影响互斥，不当错误处理。
	if err := f.Truncate(0); err == nil {
		if _, err := f.Seek(0, 0); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Sync()
		}
	}
	return &InstanceLock{f: f}, nil
}

// Release 释放锁。进程退出时操作系统也会自动释放，这里只是给测试和显式清理用。
// 不删除锁文件：删除会和另一个正在等锁的进程抢同一个路径，留着空文件更安全。
func (l *InstanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
