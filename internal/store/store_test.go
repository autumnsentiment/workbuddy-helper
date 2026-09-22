package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestConcurrentLoadAndSaveRemainConsistent(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Save(model.NewState()); err != nil {
		t.Fatal(err)
	}

	const iterations = 40
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			state := model.NewState()
			state.Accounts = []model.Account{{ID: fmt.Sprintf("a-%d", i), UID: fmt.Sprintf("u-%d", i)}}
			if err := s.Save(state); err != nil {
				errs <- fmt.Errorf("save %d: %w", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			state, err := s.Load()
			if err != nil {
				errs <- fmt.Errorf("load %d: %w", i, err)
				return
			}
			if len(state.Accounts) > 1 {
				errs <- fmt.Errorf("load %d returned a torn state: %+v", i, state.Accounts)
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestUnmarshalRejectsFutureStateVersion(t *testing.T) {
	_, err := unmarshalState([]byte(`{"version":999,"settings":{},"accounts":[]}`))
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected unsupported-version error, got %v", err)
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

func TestLoadFallsBackToValidBackupWhenPrimaryIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	state := model.NewState()
	state.Accounts = []model.Account{{ID: "a1", UID: "u1", AccessToken: "secret", Enabled: true}}
	if err := s.Save(state); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.bin.bak"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.bin"), []byte("WBK1-corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load()
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].UID != "u1" {
		t.Fatalf("valid backup was not used: state=%+v err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.bin.bak")); err != nil {
		t.Fatalf("backup should remain available after corrupt-primary recovery: %v", err)
	}
}

func TestStateSaveRejectsNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "state.bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(dir).Save(model.NewState()); err == nil {
		t.Fatal("directory state target was accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.bin.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary state file was not cleaned up: %v", err)
	}
}

func TestStateLoadRejectsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires elevated privileges on Windows")
	}
	targetDir := t.TempDir()
	if err := New(targetDir).Save(model.NewState()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(targetDir, "state.bin"), filepath.Join(dir, "state.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir).Load(); err == nil {
		t.Fatal("symlink state target was accepted")
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

func TestPortableKeyRejectsNonRegularTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses DPAPI instead of key.bin")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, keyFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := New(dir).Save(model.NewState()); err == nil {
		t.Fatal("directory key target was accepted")
	}

	if err := os.Remove(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), keyFileName)
	if err := os.WriteFile(target, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, keyFileName)); err != nil {
		t.Fatal(err)
	}
	if err := New(dir).Save(model.NewState()); err == nil {
		t.Fatal("symlink key target was accepted")
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
	if err := SaveProxyConf(src.Dir(), "direct"); err != nil {
		t.Fatal(err)
	}
	if err := SaveDNSConf(src.Dir(), []string{"9.9.9.9:53"}); err != nil {
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
	if got, err := LoadProxyConf(exportDir); err != nil || got != "" {
		t.Fatalf("exported proxy config mismatch: %q err=%v", got, err)
	}
	if got, err := LoadDNSConf(exportDir); err != nil || len(got) != 1 || got[0] != "9.9.9.9:53" {
		t.Fatalf("exported DNS config mismatch: %v err=%v", got, err)
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

func TestExportRejectsSourceDirectoryAsDestination(t *testing.T) {
	src := New(t.TempDir())
	state := model.NewState()
	state.Accounts = []model.Account{{ID: "a1", UID: "u1"}}
	if err := src.Save(state); err != nil {
		t.Fatal(err)
	}

	if _, err := ExportPortableFrom(filepath.Join(src.Dir(), "state.bin"), src.Dir()); err == nil {
		t.Fatal("expected same-directory export to be rejected")
	}
	got, err := src.Load()
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].UID != "u1" {
		t.Fatalf("source changed after rejected export: state=%+v err=%v", got, err)
	}
}

func TestImportReassignsConflictingAndEmptyAccountIDs(t *testing.T) {
	src := New(t.TempDir())
	srcState := model.NewState()
	srcState.Accounts = []model.Account{
		{ID: "shared-id", UID: "source-one"},
		{ID: "", UID: "source-two"},
	}
	if err := src.Save(srcState); err != nil {
		t.Fatal(err)
	}

	dst := New(t.TempDir())
	dstState := model.NewState()
	dstState.Accounts = []model.Account{{ID: "shared-id", UID: "destination"}}
	if err := dst.Save(dstState); err != nil {
		t.Fatal(err)
	}

	added, err := ImportInto(src.Dir(), dst.Dir())
	if err != nil || added != 2 {
		t.Fatalf("ImportInto: added=%d err=%v", added, err)
	}
	got, err := dst.Load()
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(got.Accounts))
	for _, account := range got.Accounts {
		if account.ID == "" || ids[account.ID] {
			t.Fatalf("account IDs are not unique and non-empty: %+v", got.Accounts)
		}
		ids[account.ID] = true
	}
}

func TestImportRejectsAccountLimitWithoutPartialWrite(t *testing.T) {
	src := New(t.TempDir())
	srcState := model.NewState()
	srcState.Accounts = []model.Account{{ID: "new-1", UID: "new-1"}, {ID: "new-2", UID: "new-2"}}
	if err := src.Save(srcState); err != nil {
		t.Fatal(err)
	}

	dst := New(t.TempDir())
	dstState := model.NewState()
	for i := 0; i < 49; i++ {
		dstState.Accounts = append(dstState.Accounts, model.Account{ID: fmt.Sprintf("id-%d", i), UID: fmt.Sprintf("uid-%d", i)})
	}
	if err := dst.Save(dstState); err != nil {
		t.Fatal(err)
	}

	if added, err := ImportInto(src.Dir(), dst.Dir()); err == nil || added != 0 {
		t.Fatalf("expected atomic account-limit rejection, added=%d err=%v", added, err)
	}
	got, err := dst.Load()
	if err != nil || len(got.Accounts) != 49 {
		t.Fatalf("destination changed after rejected import: accounts=%d err=%v", len(got.Accounts), err)
	}
}

func TestImportDeduplicatesUIDWithinVersionOnly(t *testing.T) {
	src := New(t.TempDir())
	srcState := model.NewState()
	srcState.Accounts = []model.Account{
		{ID: "source-cn", UID: "shared-uid", Version: model.ModeCN},
		{ID: "source-intl", UID: "shared-uid", Version: model.ModeIntl, Domain: "workbuddy.ai"},
	}
	if err := src.Save(srcState); err != nil {
		t.Fatal(err)
	}

	dst := New(t.TempDir())
	dstState := model.NewState()
	dstState.Accounts = []model.Account{{ID: "existing-cn", UID: "shared-uid", Version: model.ModeCN}}
	if err := dst.Save(dstState); err != nil {
		t.Fatal(err)
	}

	added, err := ImportInto(src.Dir(), dst.Dir())
	if err != nil || added != 1 {
		t.Fatalf("ImportInto: added=%d err=%v", added, err)
	}
	got, err := dst.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].VersionMode() != model.ModeCN || got.Accounts[1].VersionMode() != model.ModeIntl {
		t.Fatalf("cross-version account was not preserved: %+v", got.Accounts)
	}
}
