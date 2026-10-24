package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The AP only shows Wi-Fi passwords and SNMP communities as $crypt$1$ values we
// can't decrypt (and re-salts them on every display), so the app remembers every
// secret it sets. Entries are AES-256-GCM encrypted with a key kept in a
// separate file.

type SavedPassword struct {
	Label      string    `json:"ssid"` // SSID for Wi-Fi passwords ("ssid" kept for older files)
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
	SetBy      string    `json:"set_by"`
	SetAt      time.Time `json:"set_at"`
}

type secretStore struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
	data map[string]SavedPassword
}

// Entry keys. Each secret is stored under one of these.
func wlanKey(apID string, wlan int) string    { return fmt.Sprintf("%s/wlan%d", apID, wlan) }
func snmpKey(apID string, kind string) string { return fmt.Sprintf("%s/snmp-%s", apID, kind) }

func openStore(path, keyPath string) (*secretStore, error) {
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			return nil, fmt.Errorf("create key file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must hold exactly 32 bytes", keyPath)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	s := &secretStore{path: path, aead: aead, data: map[string]SavedPassword{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return s, nil
}

// Save stores secret under key k. label is a human description (the SSID for
// a Wi-Fi password).
func (s *secretStore) Save(k, label, secret, user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	// The entry key is bound in as associated data so entries can't be swapped.
	s.data[k] = SavedPassword{
		Label:      label,
		Nonce:      nonce,
		Ciphertext: s.aead.Seal(nil, nonce, []byte(secret), []byte(k)),
		SetBy:      user,
		SetAt:      time.Now(),
	}
	return s.flush()
}

func (s *secretStore) Meta(k string) (SavedPassword, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[k]
	return e, ok
}

func (s *secretStore) Reveal(k string) (SavedPassword, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[k]
	if !ok {
		return e, "", os.ErrNotExist
	}
	plain, err := s.aead.Open(nil, e.Nonce, e.Ciphertext, []byte(k))
	if err != nil {
		return e, "", fmt.Errorf("decrypt saved secret (wrong secret.key?): %w", err)
	}
	return e, string(plain), nil
}

// flush writes atomically so a crash never leaves a half-written file.
func (s *secretStore) flush() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp makes the file 0600 on Linux.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
