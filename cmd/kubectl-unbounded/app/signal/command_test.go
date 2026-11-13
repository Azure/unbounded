// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderCommand(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "check.sh"),
		[]byte("#!/usr/bin/env bash\nprintf 'ok\\n'\n"),
		0o600,
	))

	plan := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: qualification
spec:
  owner: qualification-team
  version: source-123
  runtimes:
    base: %s
  tests:
  - name: script-check
    displayName: Script check
    artifact:
      path: scripts/check.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
`, "example.invalid/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	output := &bytes.Buffer{}
	command := newRenderCommand()
	command.SetOut(output)
	command.SetArgs([]string{"--filename", planPath})
	require.NoError(t, command.Execute())
	require.Contains(t, output.String(), "kind: CertificationTest")
	require.Contains(t, output.String(), "name: script-check")
	require.Contains(t, output.String(), "content: |")
	require.Contains(t, output.String(), "digest: sha256:")
}
