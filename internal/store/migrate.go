package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"workbuddy-helper/internal/model"
)

// ExportPortableFrom decrypts a state.bin at an explicit source path (using the
// backend that protects it: DPAPI on Windows, key-file elsewhere) and writes a
// portable key-file encrypted copy into dstDir. It returns the number of
// accounts exported. Used by the native exe to prepare data for a container.
func ExportPortableFrom(srcFile, dstDir string) (int, error) {
	srcDir := filepath.Dir(srcFile)
	dstFile := filepath.Join(dstDir, "state.bin")
	if srcInfo, err := os.Stat(srcFile); err == nil {
		if dstInfo, dstErr := os.Stat(dstFile); dstErr == nil && os.SameFile(srcInfo, dstInfo) {
			return 0, fmt.Errorf("export destination must differ from the source data directory")
		}
	}
	state, err := readStateFile(srcFile)
	if err != nil {
		return 0, err
	}
	configs, err := readOptionalConfigs(srcDir)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return 0, fmt.Errorf("create destination directory: %w", err)
	}
	// Always write the portable cipher, even when running on Windows, so the
	// produced directory can be mounted into a Linux container.
	raw, err := encodeState(state, dstDir, sealPortable, fileMagicPort)
	if err != nil {
		return 0, err
	}
	if err := writeRaw(dstDir, dstFile, raw); err != nil {
		return 0, err
	}
	for name, content := range configs {
		if err := writeConfigFile(filepath.Join(dstDir, name), content); err != nil {
			return 0, fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return len(state.Accounts), nil
}

// readOptionalConfigs captures the small plaintext settings that are not part
// of encrypted state.bin. Keeping them in a portable export prevents a custom
// DNS list or an explicit "direct" proxy override from being lost during a
// Windows-to-container migration.
func readOptionalConfigs(srcDir string) (map[string][]byte, error) {
	configs := make(map[string][]byte)
	for _, name := range []string{ProxyConfFileName, DNSConfFileName} {
		path := filepath.Join(srcDir, name)
		exists, err := inspectRegularFile(path, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		configs[name] = content
	}
	return configs, nil
}

// ImportInto merges accounts from a source data directory (srcDir must contain
// state.bin, readable with the local backend) into dstDir. Accounts already
// present (matched by UID) are left untouched. It returns the number of newly
// imported accounts.
func ImportInto(srcDir, dstDir string) (int, error) {
	srcState, err := readStateFile(filepath.Join(srcDir, "state.bin"))
	if err != nil {
		return 0, fmt.Errorf("read source data: %w", err)
	}
	dst := New(dstDir)
	dstState, err := dst.Load()
	if err != nil {
		return 0, fmt.Errorf("read destination data: %w", err)
	}
	existing := make(map[string]bool, len(dstState.Accounts))
	existingIDs := make(map[string]bool, len(dstState.Accounts))
	for _, a := range dstState.Accounts {
		if a.UID != "" {
			existing[accountImportKey(a)] = true
		}
		if a.ID != "" {
			existingIDs[a.ID] = true
		}
	}
	toImport := make([]model.Account, 0, len(srcState.Accounts))
	for _, a := range srcState.Accounts {
		key := accountImportKey(a)
		if a.UID == "" || existing[key] {
			continue
		}
		if a.ID == "" || existingIDs[a.ID] {
			a.ID, err = uniqueAccountID(existingIDs)
			if err != nil {
				return 0, err
			}
		}
		existing[key] = true
		existingIDs[a.ID] = true
		toImport = append(toImport, a)
	}
	if len(toImport) == 0 {
		return 0, nil
	}
	if len(dstState.Accounts)+len(toImport) > 50 {
		return 0, fmt.Errorf("import would exceed the 50-account limit (existing %d, new %d)", len(dstState.Accounts), len(toImport))
	}
	dstState.Accounts = append(dstState.Accounts, toImport...)
	if err := dst.Save(dstState); err != nil {
		return 0, fmt.Errorf("write merged state: %w", err)
	}
	return len(toImport), nil
}

func accountImportKey(a model.Account) string {
	return a.VersionMode() + "\x00" + a.UID
}

func uniqueAccountID(existing map[string]bool) (string, error) {
	for {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate imported account id: %w", err)
		}
		id := hex.EncodeToString(raw)
		if !existing[id] {
			return id, nil
		}
	}
}
