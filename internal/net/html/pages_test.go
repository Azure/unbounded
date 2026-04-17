// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package html

import (
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
	if err != nil {
		t.Fatalf("ClusterStatusIndex() error = %v", err)
	}

	if len(content) == 0 {
		t.Fatalf("expected non-empty index content")
	}
}
