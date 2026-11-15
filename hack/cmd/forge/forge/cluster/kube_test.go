// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteBootstrapTokenManifest(t *testing.T) {
	const manifest = "apiVersion: v1\nkind: Secret\n"

	tests := map[string]struct {
		existingMode os.FileMode
	}{
		"new file":      {},
		"existing 0644": {existingMode: 0o644},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bootstrap-token.yaml")

			if tt.existingMode != 0 {
				if err := os.WriteFile(path, []byte("stale"), tt.existingMode); err != nil {
					t.Fatalf("seed existing file: %v", err)
				}
			}

			if err := writeBootstrapTokenManifest(path, []byte(manifest)); err != nil {
				t.Fatalf("write bootstrap token manifest: %v", err)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat bootstrap token manifest: %v", err)
			}

			if got := info.Mode().Perm(); got != 0o600 {
				t.Errorf("bootstrap token manifest mode = %04o, want 0600", got)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read bootstrap token manifest: %v", err)
			}

			if string(data) != manifest {
				t.Errorf("bootstrap token manifest = %q, want %q", data, manifest)
			}
		})
	}
}
