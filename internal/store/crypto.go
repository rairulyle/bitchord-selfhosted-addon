package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// backupKDFIterations follows OWASP's advice for PBKDF2-HMAC-SHA256. A file
// records its own count; counts outside minKDFIterations..maxKDFIterations are
// refused so a crafted file cannot make a restore spin for minutes.
const (
	backupKDFIterations = 600_000
	minKDFIterations    = 1_000
	maxKDFIterations    = 2_000_000
	backupKDFAlgorithm  = "pbkdf2-sha256"
)

// EncryptedBackup defines the envelope for password-encrypted snapshot backups.
type EncryptedBackup struct {
	Version    int    `json:"version"`
	Encrypted  bool   `json:"encrypted"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// IsEncryptedBackup checks whether the provided byte slice is an encrypted backup envelope.
func IsEncryptedBackup(data []byte) bool {
	var probe struct {
		Encrypted bool `json:"encrypted"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Encrypted
}

// EncryptSnapshot serializes snap to JSON and, if passphrase is non-empty,
// encrypts it using AES-256-GCM with a key derived via PBKDF2-HMAC-SHA256.
// If passphrase is empty, it returns indented plaintext JSON.
func EncryptSnapshot(snap Snapshot, passphrase string) ([]byte, error) {
	return encryptSnapshot(snap, passphrase, backupKDFIterations)
}

func encryptSnapshot(snap Snapshot, passphrase string, iterations int) ([]byte, error) {
	if passphrase == "" {
		return json.MarshalIndent(snap, "", "  ")
	}

	plain, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plain, nil)

	eb := EncryptedBackup{
		Version:    1,
		Encrypted:  true,
		KDF:        backupKDFAlgorithm,
		Iterations: iterations,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	return json.MarshalIndent(eb, "", "  ")
}

// DecryptSnapshot parses data into a Snapshot. If data is an EncryptedBackup envelope,
// it derives the key with the provided passphrase and decrypts the payload.
// If data is a legacy unencrypted Snapshot JSON, it unmarshals it directly.
func DecryptSnapshot(data []byte, passphrase string) (Snapshot, error) {
	if IsEncryptedBackup(data) {
		if passphrase == "" {
			return Snapshot{}, errors.New("backup is encrypted; password required")
		}

		var eb EncryptedBackup
		if err := json.Unmarshal(data, &eb); err != nil {
			return Snapshot{}, fmt.Errorf("invalid encrypted backup format: %w", err)
		}

		salt, err := base64.StdEncoding.DecodeString(eb.Salt)
		if err != nil || len(salt) < 8 {
			return Snapshot{}, errors.New("invalid or corrupted salt")
		}

		nonce, err := base64.StdEncoding.DecodeString(eb.Nonce)
		if err != nil {
			return Snapshot{}, errors.New("invalid or corrupted nonce")
		}

		ciphertext, err := base64.StdEncoding.DecodeString(eb.Ciphertext)
		if err != nil || len(ciphertext) == 0 {
			return Snapshot{}, errors.New("invalid or corrupted ciphertext")
		}

		if eb.KDF != backupKDFAlgorithm {
			return Snapshot{}, fmt.Errorf("unsupported key derivation %q", eb.KDF)
		}
		if eb.Iterations < minKDFIterations || eb.Iterations > maxKDFIterations {
			return Snapshot{}, fmt.Errorf("unsupported key derivation iteration count %d", eb.Iterations)
		}

		key, err := pbkdf2.Key(sha256.New, passphrase, salt, eb.Iterations, 32)
		if err != nil {
			return Snapshot{}, fmt.Errorf("derive key: %w", err)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return Snapshot{}, fmt.Errorf("new cipher: %w", err)
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return Snapshot{}, fmt.Errorf("new gcm: %w", err)
		}

		if len(nonce) != gcm.NonceSize() {
			return Snapshot{}, errors.New("invalid nonce size")
		}

		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return Snapshot{}, errors.New("incorrect password or corrupted backup file")
		}

		var snap Snapshot
		if err := json.Unmarshal(plaintext, &snap); err != nil {
			return Snapshot{}, fmt.Errorf("decrypted data is not valid configuration JSON: %w", err)
		}
		return snap, nil
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("file is not valid JSON configuration: %w", err)
	}
	return snap, nil
}
