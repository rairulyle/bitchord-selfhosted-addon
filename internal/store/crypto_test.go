package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecryptSnapshotReadsTheIterationCountTheFileRecorded(t *testing.T) {
	orig := Snapshot{PublicURL: "https://music.example.com", Servers: []Server{}}
	older, err := encryptSnapshot(orig, "pass", 100_000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptSnapshot(older, "pass")
	if err != nil || got.PublicURL != orig.PublicURL {
		t.Fatalf("100k-iteration backup: %+v, %v", got, err)
	}
	for _, iterations := range []int{minKDFIterations - 1, maxKDFIterations + 1} {
		raw, err := encryptSnapshot(orig, "pass", iterations)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecryptSnapshot(raw, "pass"); err == nil || !strings.Contains(err.Error(), "iteration count") {
			t.Errorf("%d iterations: err = %v, want an iteration count error", iterations, err)
		}
	}
}

func TestEncryptSnapshotUsesTheCurrentIterationCount(t *testing.T) {
	raw, err := EncryptSnapshot(Snapshot{Servers: []Server{}}, "pass")
	if err != nil {
		t.Fatal(err)
	}
	var envelope EncryptedBackup
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Iterations != 600_000 || envelope.KDF != "pbkdf2-sha256" {
		t.Fatalf("envelope = %+v", envelope)
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
