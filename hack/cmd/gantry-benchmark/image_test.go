// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRandomPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := writeRandomPayload(path, 1024*1024+17); err != nil {
		t.Fatalf("writeRandomPayload: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat payload: %v", err)
	}

	if info.Size() != 1024*1024+17 {
		t.Fatalf("payload size = %d, want %d", info.Size(), 1024*1024+17)
	}
}
