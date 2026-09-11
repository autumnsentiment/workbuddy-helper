package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"workbuddy-helper/internal/model"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	state := model.NewState()
	state.Accounts = append(state.Accounts, model.Account{
		ID: "a1", UID: "u1", AccessToken: "secret-access", RefreshToken: "secret-refresh", Enabled: true,
	})
	if err := s.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].RefreshToken != "secret-refresh" {
		t.Fatalf("unexpected state: %+v", got)
	}
	got.Accounts[0].RefreshToken = "rotated-refresh"
	if err := s.Save(got); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err = s.Load()
	if err != nil || got.Accounts[0].RefreshToken != "rotated-refresh" {
		t.Fatalf("second Load: state=%+v err=%v", got, err)
	}
}

func TestLoadRestoresBackupAfterInterruptedSave(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	state := model.NewState()
	state.Accounts = []model.Account{{ID: "a1", UID: "u1", AccessToken: "secret", Enabled: true}}
	if err := s.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "state.bin"), filepath.Join(dir, "state.bin.bak")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].UID != "u1" {
		t.Fatalf("state=%+v err=%v", got, err)
	}
}

func TestKeyFileCreatedOnSave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("key-file cipher is the portable backend; Windows uses DPAPI")
	}
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(model.NewState()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "key.bin")); err != nil {
		t.Fatalf("key file not created: %v", err)
	}
}

func TestExportPortableThenImport(t *testing.T) {
	src := New(t.TempDir())
	state := model.NewState()
	state.Accounts = []model.Account{
		{ID: "a1", UID: "u1", AccessToken: "tok-1", RefreshToken: "ref-1", Enabled: true},
		{ID: "a2", UID: "u2", AccessToken: "tok-2", RefreshToken: "ref-2", Enabled: true},
	}
	if err := src.Save(state); err != nil {
		t.Fatal(err)
	}
	// Export src (protected by whatever backend the platform uses) into a
	// fresh portable directory.
	exportDir := t.TempDir()
	n, err := ExportPortableFrom(filepath.Join(src.Dir(), "state.bin"), exportDir)
	if err != nil {
		t.Fatalf("ExportPortableFrom: %v", err)
	}
	if n != 2 {
		t.Fatalf("exported %d accounts, want 2", n)
	}
	// Portable directory must be readable independently (it owns its key).
	loaded, err := New(exportDir).Load()
	if err != nil {
		t.Fatalf("load exported: %v", err)
	}
	if len(loaded.Accounts) != 2 || loaded.Accounts[0].AccessToken != "tok-1" {
		t.Fatalf("exported content mismatch: %+v", loaded.Accounts)
	}
	// Import into a fresh destination directory.
	dst := New(t.TempDir())
	if err := dst.Save(model.NewState()); err != nil {
		t.Fatal(err)
	}
	added, err := ImportInto(exportDir, dst.Dir())
	if err != nil {
		t.Fatalf("ImportInto: %v", err)
	}
	if added != 2 {
		t.Fatalf("imported %d accounts, want 2", added)
	}
	// Re-importing must be a no-op (same UIDs).
	added, err = ImportInto(exportDir, dst.Dir())
	if err != nil {
		t.Fatalf("second ImportInto: %v", err)
	}
	if added != 0 {
		t.Fatalf("second import added %d accounts, want 0", added)
	}
	final, err := dst.Load()
	if err != nil || len(final.Accounts) != 2 {
		t.Fatalf("final state accounts=%d err=%v", len(final.Accounts), err)
	}
}

func TestExportRequiresSourceFile(t *testing.T) {
	if _, err := ExportPortableFrom(filepath.Join(t.TempDir(), "missing.bin"), t.TempDir()); err == nil {
		t.Fatal("expected an error for a missing source file")
	}
}
