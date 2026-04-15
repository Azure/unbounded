// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppliedConfigChecksumPath(t *testing.T) {
	tests := []struct {
		machine  string
		expected string
	}{
		{NSpawnMachineKube1, "/etc/unbounded/agent/kube1-applied-config.json.sha256"},
		{NSpawnMachineKube2, "/etc/unbounded/agent/kube2-applied-config.json.sha256"},
	}

	for _, tt := range tests {
		got := AppliedConfigChecksumPath(tt.machine)
		if got != tt.expected {
			t.Errorf("AppliedConfigChecksumPath(%q) = %q, want %q", tt.machine, got, tt.expected)
		}
	}
}

func TestComputeChecksum(t *testing.T) {
	data := []byte("hello world")
	h := sha256.Sum256(data)
	want := hex.EncodeToString(h[:])

	got := ComputeChecksum(data)
	if got != want {
		t.Errorf("ComputeChecksum(%q) = %q, want %q", data, got, want)
	}
}

func TestComputeChecksum_Empty(t *testing.T) {
	data := []byte{}
	h := sha256.Sum256(data)
	want := hex.EncodeToString(h[:])

	got := ComputeChecksum(data)
	if got != want {
		t.Errorf("ComputeChecksum(empty) = %q, want %q", got, want)
	}
}

func TestVerifyChecksum_Match(t *testing.T) {
	dir := t.TempDir()
	checksumPath := filepath.Join(dir, "config.json.sha256")

	data := []byte(`{"Version":"1.33.1"}`)
	checksum := ComputeChecksum(data)

	if err := os.WriteFile(checksumPath, []byte(checksum+"\n"), 0o600); err != nil {
		t.Fatalf("write checksum file: %v", err)
	}

	if err := VerifyChecksum(data, checksumPath); err != nil {
		t.Errorf("VerifyChecksum() returned unexpected error: %v", err)
	}
}

func TestVerifyChecksum_MissingSidecar(t *testing.T) {
	dir := t.TempDir()
	checksumPath := filepath.Join(dir, "config.json.sha256")

	data := []byte(`{"Version":"1.33.1"}`)

	// No sidecar file - should return nil (not an error).
	if err := VerifyChecksum(data, checksumPath); err != nil {
		t.Errorf("VerifyChecksum() with missing sidecar returned error: %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	dir := t.TempDir()
	checksumPath := filepath.Join(dir, "config.json.sha256")

	data := []byte(`{"Version":"1.33.1"}`)
	// Write a checksum that does not match the data.
	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(checksumPath, []byte(wrongChecksum+"\n"), 0o600); err != nil {
		t.Fatalf("write checksum file: %v", err)
	}

	err := VerifyChecksum(data, checksumPath)
	if err == nil {
		t.Fatal("VerifyChecksum() expected error for mismatched checksum, got nil")
	}

	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("VerifyChecksum() error = %v, want ErrChecksumMismatch", err)
	}
}

func TestVerifyChecksum_MatchWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	checksumPath := filepath.Join(dir, "config.json.sha256")

	data := []byte(`{"Version":"1.33.1"}`)
	checksum := ComputeChecksum(data)

	// Write checksum without trailing newline.
	if err := os.WriteFile(checksumPath, []byte(checksum), 0o600); err != nil {
		t.Fatalf("write checksum file: %v", err)
	}

	if err := VerifyChecksum(data, checksumPath); err != nil {
		t.Errorf("VerifyChecksum() with no trailing newline returned error: %v", err)
	}
}

func TestVerifyChecksum_ReadError(t *testing.T) {
	dir := t.TempDir()
	checksumPath := filepath.Join(dir, "unreadable")

	// Create a directory where we expect a file - reading will fail.
	if err := os.Mkdir(checksumPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	data := []byte(`{"Version":"1.33.1"}`)
	err := VerifyChecksum(data, checksumPath)
	if err == nil {
		t.Fatal("VerifyChecksum() expected error for unreadable path, got nil")
	}

	// Should not be ErrChecksumMismatch - it's a read error.
	if errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("VerifyChecksum() error should not be ErrChecksumMismatch for read error")
	}
}
