package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConfigFileOverwriteAndBackupRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.conf")
	if err := writeConfigFile(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "second\n" {
		t.Fatalf("overwrite failed: got=%q err=%v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte("recovered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := readConfigFile(path)
	if err != nil || string(recovered) != "recovered\n" {
		t.Fatalf("backup recovery failed: got=%q err=%v", recovered, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backup was not restored: %v", err)
	}
}

func TestConfigFileRejectsNonRegularTargets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.conf")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, []byte("direct\n")); err == nil {
		t.Fatal("directory config target was accepted")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary config file was not cleaned up: %v", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "outside.conf")
	if err := os.WriteFile(target, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, []byte("direct\n")); err == nil {
		t.Fatal("symlink config target was accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "outside\n" {
		t.Fatalf("symlink target was modified: %q err=%v", got, err)
	}
}
