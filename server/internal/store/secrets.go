package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
)

// Secrets encrypts the SNMP communities before they go into the database (AES-256-GCM). The key is a separate file (mode 0600),
// so a copy of the database alone does not give the communities.
type Secrets struct{ aead cipher.AEAD }

// LoadOrCreateKey reads the 32 byte key at path; when the file does not exist it is created with random bytes.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s must hold exactly 32 bytes, it has %d", path, len(key))
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func NewSecrets(key []byte) (*Secrets, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Secrets{aead: aead}, nil
}

// Encrypt gives nonce + ciphertext. An empty text stays empty (no secret).
func (s *Secrets) Encrypt(text string) ([]byte, error) {
	if text == "" {
		return nil, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(text), nil), nil
}

func (s *Secrets) Decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	n := s.aead.NonceSize()
	if len(blob) < n {
		return "", errors.New("secret is damaged")
	}
	plain, err := s.aead.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return "", errors.New("secret cannot be decrypted (wrong key file?)")
	}
	return string(plain), nil
}
