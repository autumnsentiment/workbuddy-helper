package main

import (
	"strings"
	"testing"

	"workbuddy-helper/internal/instance"
)

func TestDataCommandLockRejectsRunningInstance(t *testing.T) {
	dir := t.TempDir()
	serviceLock, err := instance.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer serviceLock.Close()

	if commandLock, err := acquireDataDirLock(dir); err == nil {
		commandLock.Close()
		t.Fatal("data command unexpectedly acquired a running instance's lock")
	} else if !strings.Contains(err.Error(), "已在使用") {
		t.Fatalf("unexpected lock error: %v", err)
	}
}
