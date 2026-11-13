// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	signalv1alpha1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	testRuntimeA = "example.invalid/signal-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRuntimeB = "example.invalid/vllm-runner@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigest   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestCompileFileRendersDriverTypesAndArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))

	script := []byte("#!/usr/bin/env bash\nprintf 'ok\\n'\n")
	workflowScript := []byte("#!/usr/bin/env python3\nprint('ok')\n")

	require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "check.sh"), script, 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "workflow.py"),
		workflowScript,
		0o600,
	))

	plan := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: qualification
  labels:
    app.kubernetes.io/part-of: project-signal
spec:
  sourceRoot: .
  owner: qualification-team
  version: source-123
  runtimes:
    base: %s
    vllm: %s
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
  - name: container-check
    displayName: Container check
    driver:
      type: Container
      container:
        runtime: vllm
        command: [python3]
        args: [-m, vllm]
  - name: workflow-check
    displayName: Workflow check
    artifact:
      name: workload.py
      path: scripts/workflow.py
    driver:
      type: WorkflowTemplate
      workflowTemplate:
        name: vllm-qualification
        digest: %s
`, testRuntimeA, testRuntimeB, testDigest)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	rendered, err := CompileFile(planPath)
	require.NoError(t, err)
	require.Equal(t, rendered, mustCompileFile(t, planPath))

	documents := strings.Split(strings.TrimSpace(string(rendered)), "---\n")
	require.Len(t, documents, 3)

	tests := make([]signalv1alpha1.CertificationTest, len(documents))
	for index := range documents {
		require.NoError(t, yaml.Unmarshal([]byte(documents[index]), &tests[index]))
	}

	require.Equal(t, signalv1alpha1.TestDriverTypeScript, tests[0].Spec.Driver.Type)
	require.Equal(t, testRuntimeA, tests[0].Spec.Driver.Script.RunnerImage)
	require.Equal(t, string(script), tests[0].Spec.Artifacts[0].Content)
	sum := sha256.Sum256(script)
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), tests[0].Spec.Artifacts[0].Digest)
	require.Equal(t, "qualification", tests[0].Annotations["signal.unbounded-cloud.io/source-plan"])
	require.Equal(t, int32(1), tests[0].Spec.Topology.MinParticipants)
	require.Equal(t, int32(1), tests[0].Spec.Topology.MaxParticipants)
	require.Equal(t, "scriptResult", tests[0].Spec.Metrics[0].Name)

	require.Equal(t, signalv1alpha1.TestDriverTypeContainer, tests[1].Spec.Driver.Type)
	require.Equal(t, testRuntimeB, tests[1].Spec.Driver.Container.Image)
	require.Empty(t, tests[1].Spec.Artifacts)

	require.Equal(t, signalv1alpha1.TestDriverTypeWorkflowTemplate, tests[2].Spec.Driver.Type)
	require.Equal(t, "vllm-qualification", tests[2].Spec.Driver.WorkflowTemplate.Name)
	require.Equal(t, "workload.py", tests[2].Spec.Driver.WorkflowTemplate.Artifact)
	require.Equal(t, string(workflowScript), tests[2].Spec.Artifacts[0].Content)
}

func TestCompileFileRejectsUnsafeOrInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sourceRoot  string
		artifact    string
		runtime     string
		extra       string
		wantError   string
		writeSource bool
	}{
		{
			name:        "source escapes root",
			sourceRoot:  "scripts",
			artifact:    "../outside.sh",
			runtime:     testRuntimeA,
			wantError:   "escapes spec.sourceRoot",
			writeSource: true,
		},
		{
			name:        "runtime is mutable",
			sourceRoot:  ".",
			artifact:    "scripts/check.sh",
			runtime:     "example.invalid/runner:latest",
			wantError:   "must be a digest-pinned image",
			writeSource: true,
		},
		{
			name:        "unknown field",
			sourceRoot:  ".",
			artifact:    "scripts/check.sh",
			runtime:     testRuntimeA,
			extra:       "  unexpected: true\n",
			wantError:   `unknown field "unexpected"`,
			writeSource: true,
		},
		{
			name:        "missing source",
			sourceRoot:  ".",
			artifact:    "scripts/missing.sh",
			runtime:     testRuntimeA,
			wantError:   "resolve artifact.path",
			writeSource: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))

			if tt.writeSource {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "outside.sh"),
					[]byte("outside\n"),
					0o600,
				))
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "scripts", "check.sh"),
					[]byte("check\n"),
					0o600,
				))
			}

			plan := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: qualification
spec:
  sourceRoot: %s
  owner: qualification-team
  version: source-123
  runtimes:
    base: %s
%s  tests:
  - name: script-check
    displayName: Script check
    artifact:
      path: %s
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
`, tt.sourceRoot, tt.runtime, tt.extra, tt.artifact)
			planPath := filepath.Join(root, "certification-plan.yaml")
			require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

			_, err := CompileFile(planPath)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestCompileFileRejectsOversizedArtifact(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "large.sh"),
		make([]byte, signalv1alpha1.MaxTestArtifactBytes+1),
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
      path: scripts/large.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
`, testRuntimeA)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	_, err := CompileFile(planPath)
	require.ErrorContains(t, err, fmt.Sprintf("exceeds the %d-byte limit", signalv1alpha1.MaxTestArtifactBytes))
}

func mustCompileFile(t *testing.T, path string) []byte {
	t.Helper()

	rendered, err := CompileFile(path)
	require.NoError(t, err)

	return rendered
}
