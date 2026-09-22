//go:build solaris || aix

package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Lock uses an advisory fcntl record lock on platforms whose syscall package
// does not expose BSD flock (currently Solaris and AIX). The lock is released
// automatically when the file descriptor closes or the process exits.
type Lock struct {
	f    *os.File
	path string
}

// POSIX fcntl record locks are process-scoped, so a second descriptor opened
// by this process would otherwise be allowed to acquire the same lock. Keep a
// small in-process guard to preserve the same-directory behavior as flock and
// the Windows implementation.
var (
	processLocksMu sync.Mutex
	processLocks   = make(map[string]struct{})
)

func Acquire(dataDir string) (*Lock, error) {
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := filepath.Join(absDir, "instance.lock")
	processLocksMu.Lock()
	if _, exists := processLocks[lockPath]; exists {
		processLocksMu.Unlock()
		return nil, fmt.Errorf("WorkBuddy Helper 已在使用这个数据目录运行")
	}
	processLocks[lockPath] = struct{}{}
	processLocksMu.Unlock()

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		processLocksMu.Lock()
		delete(processLocks, lockPath)
		processLocksMu.Unlock()
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	lock := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: 0,
		Start:  0,
		Len:    0,
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		_ = f.Close()
		processLocksMu.Lock()
		delete(processLocks, lockPath)
		processLocksMu.Unlock()
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EDEADLK) {
			return nil, fmt.Errorf("WorkBuddy Helper 已在使用这个数据目录运行")
		}
		return nil, fmt.Errorf("lock instance directory: %w", err)
	}
	return &Lock{f: f, path: lockPath}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlock := syscall.Flock_t{
		Type:   syscall.F_UNLCK,
		Whence: 0,
		Start:  0,
		Len:    0,
	}
	unlockErr := syscall.FcntlFlock(l.f.Fd(), syscall.F_SETLK, &unlock)
	closeErr := l.f.Close()
	processLocksMu.Lock()
	delete(processLocks, l.path)
	processLocksMu.Unlock()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
