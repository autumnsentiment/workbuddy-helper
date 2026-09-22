package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
	errFutureState  = errors.New("state version is newer than supported")
)

type Store struct {
	dir  string
	path string
	mu   sync.Mutex
}

func New(dir string) *Store {
	return &Store{dir: dir, path: filepath.Join(dir, "state.bin")}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Load() (model.State, error) {
	// Load may restore state.bin.bak when a previous save was interrupted.
	// Serialize it with Save so a reader cannot mistake Save's short
	// path -> backup -> replacement window for an interrupted write.
	s.mu.Lock()
	defer s.mu.Unlock()

	exists, inspectErr := inspectRegularFile(s.path, "state file")
	if inspectErr != nil {
		return model.State{}, inspectErr
	}
	var primaryErr error
	if exists {
		raw, readErr := os.ReadFile(s.path)
		if readErr == nil {
			state, decodeErr := decodeState(raw, s.dir)
			if decodeErr == nil {
				return normalize(state), nil
			}
			// A complete backup can be newer than a truncated/corrupt primary
			// after a process crash. Keep the primary error for diagnostics but
			// continue below and try the backup before failing the load.
			primaryErr = decodeErr
			if errors.Is(primaryErr, errFutureState) {
				// Do not silently downgrade a state created by a newer binary to
				// an older backup; the caller must upgrade instead.
				return model.State{}, primaryErr
			}
		} else {
			primaryErr = fmt.Errorf("read state: %w", readErr)
		}
	}

	backup := s.path + ".bak"
	backupExists, backupInspectErr := inspectRegularFile(backup, "state backup")
	if backupInspectErr != nil {
		return model.State{}, backupInspectErr
	}
	if !backupExists {
		if !exists {
			return model.NewState(), nil
		}
		return model.State{}, primaryErr
	}

	backupRaw, err := os.ReadFile(backup)
	if err != nil {
		if primaryErr != nil {
			return model.State{}, fmt.Errorf("%v; read backup state: %w", primaryErr, err)
		}
		return model.State{}, fmt.Errorf("read backup state: %w", err)
	}
	backupState, backupErr := decodeState(backupRaw, s.dir)
	if backupErr != nil {
		if primaryErr != nil {
			return model.State{}, fmt.Errorf("%v; backup state is also invalid: %w", primaryErr, backupErr)
		}
		return model.State{}, fmt.Errorf("decode backup state: %w", backupErr)
	}

	if !exists {
		// A crash between staging the old file and renaming the new file leaves
		// the backup as the only recoverable copy. Promote it only after the
		// ciphertext and JSON have been validated.
		if err := os.Rename(backup, s.path); err != nil {
			return model.State{}, fmt.Errorf("restore backup state: %w", err)
		}
	}
	// If the primary exists but is corrupt, leave it in place for forensic
	// recovery. The next successful Save will atomically replace it; returning
	// the validated backup here keeps the service available after a crash.
	return normalize(backupState), nil
}

func (s *Store) Save(state model.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	if state.Version > model.StateVersion {
		return model.State{}, fmt.Errorf("%w: %d is newer than supported version %d; upgrade WorkBuddy Helper before opening this data", errFutureState, state.Version, model.StateVersion)
	}
	return normalize(state), nil
}

// inspectRegularFile uses Lstat deliberately: a symlink is not an acceptable
// state/config target because writing through it could modify a file outside
// the data directory. Existing regular files may be replaced atomically.
func inspectRegularFile(path, label string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", label)
	}
	return true, nil
}

func removeRegularFile(path, label string) error {
	exists, err := inspectRegularFile(path, label)
	if err != nil || !exists {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", label, err)
	}
	return nil
}

// writeRaw atomically writes raw bytes to path under dir.
func writeRaw(dir, path string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	tmp := path + ".tmp"
	if _, err := inspectRegularFile(tmp, "state temporary file"); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	backup := path + ".bak"
	hadOriginal, err := inspectRegularFile(path, "state file")
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := removeRegularFile(backup, "state backup"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if hadOriginal {
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
	if exists, err := inspectRegularFile(path, "state file"); err != nil {
		return model.State{}, err
	} else if !exists {
		return model.State{}, fmt.Errorf("state file does not exist")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.State{}, fmt.Errorf("read state: %w", err)
	}
	return decodeState(raw, filepath.Dir(path))
}
