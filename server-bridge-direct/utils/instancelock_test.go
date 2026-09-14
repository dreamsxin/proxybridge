package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireInstanceLockRejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db.lock")

	first, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	// 第二次用的是另一个 open file description / 另一个句柄，
	// 和真实的第二个进程等价。
	second, err := AcquireInstanceLock(path)
	if !errors.Is(err, ErrInstanceLocked) {
		if second != nil {
			second.Release()
		}
		t.Fatalf("second acquire err = %v, want ErrInstanceLocked", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db.lock")

	first, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	second.Release()
}

func TestInstanceLockRecordsPid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db.lock")

	lock, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("lock file content %q is not a pid: %v", content, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("lock file pid = %d, want %d", pid, os.Getpid())
	}
}
