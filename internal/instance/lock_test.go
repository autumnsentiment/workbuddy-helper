//go:build windows || unix

package instance

import "testing"

func TestAcquireRejectsSameDirectoryTwice(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Acquire(dir); err == nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
}

func TestAcquireAllowsReleaseAndReacquire(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
	second.Close()
}
