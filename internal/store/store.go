package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"workbuddy-helper/internal/model"
)

// State file magic prefixes:
//   - WBH1: DPAPI-protected (Windows native data directory)
//   - WBK1: portable AES-GCM protected with key.bin from the data directory
//
// New writes use the platform default cipher (see seal_default_*.go). Reads
// accept either prefix, so a Windows exe can read key-file data and a Linux
// container can read DPAPI data only after the user exports it with the
// portable cipher (DPAPI bytes are tied to the Windows user/machine).
var (
	fileMagicLegacy = []byte("WBH1")
	fileMagicPort   = []byte("WBK1")
)

type Store struct {
	dir  string
	path string
}

func New(dir string) *Store {
	return &Store{dir: dir, path: filepath.Join(dir, "state.bin")}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Load() (model.State, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		backup := s.path + ".bak"
		raw, err = os.ReadFile(backup)
		if errors.Is(err, os.ErrNotExist) {
			return model.NewState(), nil
		}
		if err != nil {
			return model.State{}, fmt.Errorf("read backup state: %w", err)
		}
		// A crash between staging the old file and renaming the new file leaves
		// the backup as the only recoverable copy.
		if err := os.Rename(backup, s.path); err != nil {
			return model.State{}, fmt.Errorf("restore backup state: %w", err)
		}
	}
	if err != nil {
		return model.State{}, fmt.Errorf("read state: %w", err)
	}
	state, err := decodeState(raw, s.dir)
	if err != nil {
		return model.State{}, err
	}
	return normalize(state), nil
}

func (s *Store) Save(state model.State) error {
	raw, err := encodeState(state, s.dir, platformSeal(), platformMagic())
	if err != nil {
		return err
	}
	return writeRaw(s.dir, s.path, raw)
}

func normalize(state model.State) model.State {
	if state.Version == 0 {
		state.Version = model.StateVersion
	}
	if state.Settings.ScheduleTime == "" {
		state.Settings.ScheduleTime = "09:15"
	}
	if state.Accounts == nil {
		state.Accounts = []model.Account{}
	}
	if state.Logs == nil {
		state.Logs = []model.LogEntry{}
	}
	return state
}

// encodeState marshals the state, seals the plaintext with seal(plain, dir)
// and prefixes the given file magic.
func encodeState(state model.State, dir string, seal func([]byte, string) ([]byte, error), magic []byte) ([]byte, error) {
	state.Version = model.StateVersion
	plain, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	sealed, err := seal(plain, dir)
	if err != nil {
		return nil, fmt.Errorf("encrypt state: %w", err)
	}
	return append(append([]byte(nil), magic...), sealed...), nil
}

// decodeState validates the magic prefix and decrypts with the cipher that
// produced it.
func decodeState(raw []byte, dir string) (model.State, error) {
	var sealed []byte
	switch {
	case bytes.HasPrefix(raw, fileMagicPort):
		sealed = raw[len(fileMagicPort):]
		plain, err := openPortable(sealed, dir)
		if err != nil {
			return model.State{}, fmt.Errorf("decrypt state: %w", err)
		}
		return unmarshalState(plain)
	case bytes.HasPrefix(raw, fileMagicLegacy):
		sealed = raw[len(fileMagicLegacy):]
		plain, err := openDPAPI(sealed)
		if err != nil {
			return model.State{}, fmt.Errorf("decrypt state: %w", err)
		}
		return unmarshalState(plain)
	default:
		return model.State{}, fmt.Errorf("state file format is invalid")
	}
}

func unmarshalState(plain []byte) (model.State, error) {
	var state model.State
	if err := json.Unmarshal(plain, &state); err != nil {
		return model.State{}, fmt.Errorf("parse state: %w", err)
	}
	return normalize(state), nil
}

// writeRaw atomically writes raw bytes to path under dir.
func writeRaw(dir, path string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	backup := path + ".bak"
	_ = os.Remove(backup)
	if _, err := os.Stat(path); err == nil {
		if err := os.Rename(path, backup); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("stage old state: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		_ = os.Rename(backup, path)
		return fmt.Errorf("replace state: %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

// readStateFile reads a state file at an explicit path (used by --export /
// --import tooling) without touching the caller's own data directory.
func readStateFile(path string) (model.State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.State{}, fmt.Errorf("read state: %w", err)
	}
	return decodeState(raw, filepath.Dir(path))
}
