package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const keyFileName = "key.bin"

// openPortable decrypts a key-file sealed blob. It loads (or, when saving,
// creates) the installation-local 256-bit key from dir/key.bin.
func openPortable(sealed []byte, dir string) ([]byte, error) {
	key, err := loadKey(dir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	if len(sealed) < aead.NonceSize() {
		return nil, fmt.Errorf("ciphertext is too short")
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("key-file decrypt failed (key mismatch or corrupt data)")
	}
	return plain, nil
}

// sealPortable encrypts plaintext with the key-file cipher, creating key.bin
// in dir when it does not yet exist.
func sealPortable(plain []byte, dir string) ([]byte, error) {
	key, err := loadOrCreateKey(dir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plain, nil), nil
}

func loadKey(dir string) ([]byte, error) {
	key, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		return nil, fmt.Errorf("read key file %s (needed to decrypt this data): %w", filepath.Join(dir, keyFileName), err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key file %s has an invalid size", filepath.Join(dir, keyFileName))
	}
	return key, nil
}

func loadOrCreateKey(dir string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	keyPath := filepath.Join(dir, keyFileName)
	if key, err := os.ReadFile(keyPath); err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("key file %s has an invalid size", keyPath)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}
