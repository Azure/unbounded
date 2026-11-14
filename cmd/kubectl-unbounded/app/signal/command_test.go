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
    parameters:
    - name: environmentJson
      displayName: Environment
      type: String
      required: true
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

func TestRenderCommandSupportsSuiteBootstrapAndBindings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "check.sh"),
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
      path: check.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
  suite:
    name: qualification
    phases:
    - name: validation
      role: Validation
      stages:
      - name: script-check
        test: script-check
        parameters:
        - name: environmentJson
          value:
            type: String
            stringValue: '{"EXPECTED":"base"}'
        schedule:
          mode: RunOnce
          invocationTimeout: 5m
`, "example.invalid/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	bindingsPath := filepath.Join(root, "certification-bindings.yaml")
	require.NoError(t, os.WriteFile(bindingsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: qualification
spec:
  tests:
    script-check:
      name: script-check
      uid: 11111111-1111-1111-1111-111111111111
      generation: 1
      contentDigest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
`), 0o600))

	inputsPath := filepath.Join(root, "certification-inputs.yaml")
	require.NoError(t, os.WriteFile(inputsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanInputs
metadata:
  name: qualification
spec:
  stages:
    script-check:
      parameters:
      - name: environmentJson
        value:
          type: String
          stringValue: '{"EXPECTED":"override"}'
`), 0o600))

	testsOutput := &bytes.Buffer{}
	testsCommand := newRenderCommand()
	testsCommand.SetOut(testsOutput)
	testsCommand.SetArgs([]string{"--filename", planPath, "--tests-only"})
	require.NoError(t, testsCommand.Execute())
	require.Contains(t, testsOutput.String(), "kind: CertificationTest")
	require.NotContains(t, testsOutput.String(), "kind: CertificationSuite")

	packageOutput := &bytes.Buffer{}
	packageCommand := newRenderCommand()
	packageCommand.SetOut(packageOutput)
	packageCommand.SetArgs([]string{
		"--filename", planPath,
		"--bindings", bindingsPath,
		"--inputs", inputsPath,
	})
	require.NoError(t, packageCommand.Execute())
	require.Contains(t, packageOutput.String(), "kind: CertificationTest")
	require.Contains(t, packageOutput.String(), "kind: CertificationSuite")
	require.Contains(t, packageOutput.String(), `stringValue: '{"EXPECTED":"override"}'`)

	inputsOnlyCommand := newRenderCommand()
	inputsOnlyCommand.SetArgs([]string{"--filename", planPath, "--inputs", inputsPath})
	require.EqualError(t, inputsOnlyCommand.Execute(), "--inputs requires --bindings")
}
