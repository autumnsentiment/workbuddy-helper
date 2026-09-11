package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExportPortableFrom decrypts a state.bin at an explicit source path (using the
// backend that protects it: DPAPI on Windows, key-file elsewhere) and writes a
// portable key-file encrypted copy into dstDir. It returns the number of
// accounts exported. Used by the native exe to prepare data for a container.
func ExportPortableFrom(srcFile, dstDir string) (int, error) {
	state, err := readStateFile(srcFile)
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
	if err := writeRaw(dstDir, filepath.Join(dstDir, "state.bin"), raw); err != nil {
		return 0, err
	}
	return len(state.Accounts), nil
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
	for _, a := range dstState.Accounts {
		existing[a.UID] = true
	}
	added := 0
	for _, a := range srcState.Accounts {
		if a.UID == "" || existing[a.UID] {
			continue
		}
		dstState.Accounts = append(dstState.Accounts, a)
		existing[a.UID] = true
		added++
	}
	if added == 0 {
		return 0, nil
	}
	if err := dst.Save(dstState); err != nil {
		return 0, fmt.Errorf("write merged state: %w", err)
	}
	return added, nil
}
