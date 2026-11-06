// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestDigest(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "objects.yaml")

	content := `apiVersion: v1
kind: ConfigMap
metadata:
  name: scripts
data:
  test.sh: |
    echo test
---
apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: canary
spec:
  entrypoint: test
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	dataDigest, err := manifestDigest(path, "ConfigMap", "scripts", "test.sh", "")
	if err != nil {
		t.Fatalf("digest ConfigMap data: %v", err)
	}

	if dataDigest != "sha256:056302317aae93b3c0cfcf9b2d8300c6f77fca580d1848d229799cc4edd47901" {
		t.Fatalf("ConfigMap data digest = %s", dataDigest)
	}

	fieldDigest, err := manifestDigest(path, "WorkflowTemplate", "canary", "", "spec")
	if err != nil {
		t.Fatalf("digest WorkflowTemplate spec: %v", err)
	}

	if fieldDigest != "sha256:593b242bb18356e0d2613911f8dd6e4e42916d228202f9799909bc004e2a5fc4" {
		t.Fatalf("WorkflowTemplate spec digest = %s", fieldDigest)
	}
}
