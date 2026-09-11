//go:build windows

package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

type Lock struct {
	handle syscall.Handle
}

func Acquire(dataDir string) (*Lock, error) {
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lockPath := filepath.Join(absDir, "instance.lock")
	lockPathPtr, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, fmt.Errorf("instance lock path: %w", err)
	}
	handle, err := syscall.CreateFile(
		lockPathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_HIDDEN,
		0,
	)
	if err != nil {
		if errno, ok := err.(syscall.Errno); ok && (errno == errorSharingViolation || errno == errorLockViolation) {
			return nil, fmt.Errorf("WorkBuddy Helper 已在使用这个数据目录运行")
		}
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	return &Lock{handle: handle}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(l.handle)
	l.handle = 0
	return err
}
