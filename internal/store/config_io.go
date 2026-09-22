package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Config files are small but are read and written independently of state.bin.
// Serialize them across proxy/DNS helpers so two settings operations cannot
// reuse the same temporary or backup path at the same time.
var configFileMu sync.Mutex

func readConfigFile(path string) ([]byte, error) {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	exists, err := inspectRegularFile(path, "config file")
	if err != nil {
		return nil, err
	}
	if !exists {
		backupExists, backupErr := inspectRegularFile(path+".bak", "config backup")
		if backupErr != nil {
			return nil, backupErr
		}
		if !backupExists {
			return nil, os.ErrNotExist
		}
		raw, backupErr := os.ReadFile(path + ".bak")
		if backupErr != nil {
			return nil, backupErr
		}
		if err := os.Rename(path+".bak", path); err != nil {
			return nil, fmt.Errorf("restore config backup: %w", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, nil
	}
	return nil, err
}

func writeConfigFile(path string, raw []byte) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp := path + ".tmp"
	if _, err := inspectRegularFile(tmp, "config temporary file"); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	backup := path + ".bak"
	if err := removeRegularFile(backup, "config backup"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	hadOriginal, err := inspectRegularFile(path, "config file")
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if hadOriginal {
		// Moving the old file first makes replacement work on Windows, where
		// os.Rename cannot replace an existing destination.
		if err := os.Rename(path, backup); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("stage old config: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if hadOriginal {
			_ = os.Rename(backup, path)
		}
		return fmt.Errorf("replace config: %w", err)
	}
	if hadOriginal {
		_ = os.Remove(backup)
	}
	return nil
}
