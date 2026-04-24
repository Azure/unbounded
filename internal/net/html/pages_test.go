// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package html

import (
	"errors"
	"io/fs"
	"testing"
)

// TestClusterStatusFS tests cluster status fs.
func TestClusterStatusFS(t *testing.T) {
	assetsFS, err := ClusterStatusFS()
	if err != nil {
		t.Fatalf("ClusterStatusFS() error = %v", err)
	}

	content, err := fs.ReadFile(assetsFS, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("frontend not built; run `make net-frontend` to enable this test")
	}

	if err != nil {
		t.Fatalf("read index.html from embedded fs: %v", err)
	}

	if len(content) == 0 {
		t.Fatalf("expected non-empty index.html content")
	}
}

// TestClusterStatusIndex tests cluster status index.
func TestClusterStatusIndex(t *testing.T) {
	content, err := ClusterStatusIndex()
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("frontend not built; run `make net-frontend` to enable this test")
	}

	if err != nil {
		t.Fatalf("ClusterStatusIndex() error = %v", err)
	}

	if len(content) == 0 {
		t.Fatalf("expected non-empty index content")
	}
}
