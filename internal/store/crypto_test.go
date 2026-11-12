package store

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestPBKDF2SHA256(t *testing.T) {
	// Standard test: password="password", salt="salt", iterations=4096, keyLen=32
	// Expected output for PBKDF2-HMAC-SHA256:
	// c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a
	pass := []byte("password")
	salt := []byte("salt")
	key := pbkdf2SHA256(pass, salt, 4096, 32)
	got := hex.EncodeToString(key)
	want := "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"
	if got != want {
		t.Fatalf("PBKDF2 got %s, want %s", got, want)
	}
}

func TestEncryptDecryptSnapshotRoundTrip(t *testing.T) {
	orig := Snapshot{
		PublicURL: "https://music.example.com",
		Servers: []Server{
			{
				Slug:    "plex-1",
				Kind:    Plex,
				Label:   "Main Plex",
				URL:     "http://localhost:32400",
				Token:   "secret-token-xyz",
				Library: "1",
				Enabled: true,
			},
		},
	}

	// 1. Unencrypted roundtrip
	plainData, err := EncryptSnapshot(orig, "")
	if err != nil {
		t.Fatalf("EncryptSnapshot unencrypted failed: %v", err)
	}
	if IsEncryptedBackup(plainData) {
		t.Error("expected IsEncryptedBackup to be false for unencrypted data")
	}
	restoredPlain, err := DecryptSnapshot(plainData, "")
	if err != nil {
		t.Fatalf("DecryptSnapshot unencrypted failed: %v", err)
	}
	if restoredPlain.PublicURL != orig.PublicURL || len(restoredPlain.Servers) != 1 || restoredPlain.Servers[0].Token != "secret-token-xyz" {
		t.Errorf("unencrypted roundtrip mismatch: %+v", restoredPlain)
	}

	// 2. Encrypted roundtrip with passphrase
	pass := "super-secret-backup-passphrase-123"
	encData, err := EncryptSnapshot(orig, pass)
	if err != nil {
		t.Fatalf("EncryptSnapshot encrypted failed: %v", err)
	}
	if !IsEncryptedBackup(encData) {
		t.Error("expected IsEncryptedBackup to be true for encrypted data")
	}
	if strings.Contains(string(encData), "secret-token-xyz") {
		t.Error("plaintext token found in encrypted payload!")
	}

	// Decrypt with correct passphrase
	restoredEnc, err := DecryptSnapshot(encData, pass)
	if err != nil {
		t.Fatalf("DecryptSnapshot with correct password failed: %v", err)
	}
	if restoredEnc.PublicURL != orig.PublicURL || len(restoredEnc.Servers) != 1 || restoredEnc.Servers[0].Token != "secret-token-xyz" {
		t.Errorf("encrypted roundtrip mismatch: %+v", restoredEnc)
	}

	// Decrypt with missing passphrase
	_, err = DecryptSnapshot(encData, "")
	if err == nil || !strings.Contains(err.Error(), "password required") {
		t.Errorf("expected password required error, got: %v", err)
	}

	// Decrypt with wrong passphrase
	_, err = DecryptSnapshot(encData, "wrong-passphrase")
	if err == nil || !strings.Contains(err.Error(), "incorrect password") {
		t.Errorf("expected incorrect password error, got: %v", err)
	}

	// Decrypt corrupted data
	_, err = DecryptSnapshot([]byte("{ not valid json }"), "")
	if err == nil {
		t.Error("expected error for corrupted json, got nil")
	}
}
