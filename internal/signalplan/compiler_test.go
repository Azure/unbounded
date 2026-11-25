// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalplan

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	config := []byte("{\"mode\":\"qualification\"}\n")
	workflowScript := []byte("#!/usr/bin/env python3\nprint('ok')\n")

	require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "check.sh"), script, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "config.json"), config, 0o600))
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
    artifacts:
    - path: scripts/check.sh
    - path: scripts/config.json
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
        artifact: check.sh
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
	require.Equal(t, "check.sh", tests[0].Spec.Driver.Script.Artifact)
	require.Equal(t, string(script), tests[0].Spec.Artifacts[0].Content)
	require.Equal(t, string(config), tests[0].Spec.Artifacts[1].Content)

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

func TestCompileFileWithBindingsRendersOrderedSuite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "preflight.sh"),
		[]byte("#!/usr/bin/env bash\nprintf 'preflight\\n'\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "workload.sh"),
		[]byte("#!/usr/bin/env bash\nprintf 'workload\\n'\n"),
		0o600,
	))

	plan := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: node-certification
  labels:
    app.kubernetes.io/part-of: project-signal
spec:
  owner: qualification-team
  version: source-123
  runtimes:
    base: %s
  tests:
  - name: hardware-preflight
    displayName: Hardware preflight
    artifact:
      path: scripts/preflight.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
  - name: gpu-workload
    displayName: GPU workload
    artifact:
      path: scripts/workload.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
  suite:
    name: node-certification
    phases:
    - name: validation
      displayName: Ordered validation
      role: Validation
      stages:
      - name: preflight
        test: hardware-preflight
        schedule:
          mode: RunOnce
          invocationTimeout: 5m
      - name: workload
        test: gpu-workload
        schedule:
          mode: RunOnce
          invocationTimeout: 30m
        dependsOn:
        - stage: preflight
`, testRuntimeA)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	bindings := fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: node-certification
spec:
  tests:
    hardware-preflight:
      name: hardware-preflight
      uid: %s
      generation: 1
      contentDigest: %s
    gpu-workload:
      name: gpu-workload
      uid: %s
      generation: 2
      contentDigest: %s
`, "11111111-1111-1111-1111-111111111111", testDigest,
		"22222222-2222-2222-2222-222222222222", testDigest)
	bindingsPath := filepath.Join(root, "certification-bindings.yaml")
	require.NoError(t, os.WriteFile(bindingsPath, []byte(bindings), 0o600))

	_, err := CompileFile(planPath)
	require.ErrorContains(t, err, "spec.suite requires --bindings")

	testsOnly, err := CompileTestsFile(planPath)
	require.NoError(t, err)
	require.NotContains(t, string(testsOnly), "kind: CertificationSuite")

	rendered, err := CompileFileWithBindings(planPath, bindingsPath)
	require.NoError(t, err)
	require.Equal(t, rendered, mustCompileFileWithBindings(t, planPath, bindingsPath))

	documents := strings.Split(strings.TrimSpace(string(rendered)), "---\n")
	require.Len(t, documents, 3)

	var suite signalv1alpha1.CertificationSuite
	require.NoError(t, yaml.Unmarshal([]byte(documents[2]), &suite))
	require.Equal(t, "node-certification", suite.Name)
	require.Equal(
		t,
		"node-certification",
		suite.Annotations["signal.unbounded-cloud.io/source-plan"],
	)
	require.Equal(t, "project-signal", suite.Labels["app.kubernetes.io/part-of"])
	require.Len(t, suite.Spec.Phases, 1)
	require.Len(t, suite.Spec.Phases[0].Stages, 2)
	require.Equal(
		t,
		"11111111-1111-1111-1111-111111111111",
		string(suite.Spec.Phases[0].Stages[0].Test.UID),
	)
	require.Equal(
		t,
		"preflight",
		suite.Spec.Phases[0].Stages[1].DependsOn[0].Stage,
	)
}

func TestCompileFileWithBindingsAndInputsOverridesStageParameters(t *testing.T) {
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
    parameters:
    - name: environmentJson
      displayName: Execution environment
      type: String
      required: true
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
      - name: check
        test: script-check
        parameters:
        - name: environmentJson
          value:
            type: String
            stringValue: '{"duration":"short"}'
        schedule:
          mode: RunOnce
          invocationTimeout: 5m
`, testRuntimeA)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	bindingsPath := filepath.Join(root, "certification-bindings.yaml")
	require.NoError(t, os.WriteFile(bindingsPath, []byte(fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: qualification
spec:
  tests:
    script-check:
      name: script-check
      uid: 11111111-1111-1111-1111-111111111111
      generation: 1
      contentDigest: %s
`, testDigest)), 0o600))

	inputsPath := filepath.Join(root, "certification-inputs.yaml")
	require.NoError(t, os.WriteFile(inputsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanInputs
metadata:
  name: qualification
spec:
  stages:
    check:
      parameters:
      - name: environmentJson
        value:
          type: String
          stringValue: '{"duration":"long"}'
`), 0o600))

	rendered, err := CompileFileWithBindingsAndInputs(planPath, bindingsPath, inputsPath)
	require.NoError(t, err)

	documents := strings.Split(strings.TrimSpace(string(rendered)), "---\n")
	require.Len(t, documents, 2)

	var suite signalv1alpha1.CertificationSuite
	require.NoError(t, yaml.Unmarshal([]byte(documents[1]), &suite))
	require.Len(t, suite.Spec.Phases[0].Stages[0].Parameters, 1)
	require.Equal(
		t,
		`{"duration":"long"}`,
		*suite.Spec.Phases[0].Stages[0].Parameters[0].Value.StringValue,
	)

	require.NoError(t, os.WriteFile(inputsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanInputs
metadata:
  name: qualification
spec:
  stages:
    unknown:
      parameters: []
`), 0o600))
	_, err = CompileFileWithBindingsAndInputs(planPath, bindingsPath, inputsPath)
	require.ErrorContains(t, err, `inputs spec.stages contains undefined stage "unknown"`)

	require.NoError(t, os.WriteFile(inputsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanInputs
metadata:
  name: qualification
spec:
  stages:
    check:
      parameters:
      - name: environmentJson
        value:
          type: String
          stringValue: first
      - name: environmentJson
        value:
          type: String
          stringValue: second
`), 0o600))
	_, err = CompileFileWithBindingsAndInputs(planPath, bindingsPath, inputsPath)
	require.ErrorContains(t, err, `input parameter "environmentJson" is duplicated`)
}

func TestCompileFileWithBindingsRejectsMissingIdentityAndDependencyCycle(t *testing.T) {
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
      - name: first
        test: script-check
        schedule: {mode: RunOnce, invocationTimeout: 5m}
        dependsOn: [{stage: second}]
      - name: second
        test: script-check
        schedule: {mode: RunOnce, invocationTimeout: 5m}
        dependsOn: [{stage: first}]
`, testRuntimeA)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	bindingsPath := filepath.Join(root, "certification-bindings.yaml")
	require.NoError(t, os.WriteFile(bindingsPath, []byte(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: qualification
spec:
  tests: {}
`), 0o600))

	_, err := CompileFileWithBindings(planPath, bindingsPath)
	require.ErrorContains(t, err, `bindings spec.tests["script-check"] is required`)

	require.NoError(t, os.WriteFile(bindingsPath, []byte(fmt.Sprintf(`apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlanBindings
metadata:
  name: qualification
spec:
  tests:
    script-check:
      name: script-check
      uid: 11111111-1111-1111-1111-111111111111
      generation: 1
      contentDigest: %s
`, testDigest)), 0o600))

	_, err = CompileFileWithBindings(planPath, bindingsPath)
	require.ErrorContains(t, err, "stage dependency graph contains a cycle")
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
			wantError:   "resolve path",
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

func TestCompileFileRejectsInvalidArtifactSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		artifacts string
		driver    string
		wantError string
	}{
		{
			name: "singular and plural",
			artifacts: `    artifact:
      path: first.sh
    artifacts:
    - path: second.sh
`,
			driver: `      script:
        language: Shell
        runtime: base
`,
			wantError: "artifact and artifacts are mutually exclusive",
		},
		{
			name: "multiple without selection",
			artifacts: `    artifacts:
    - path: first.sh
    - path: second.sh
`,
			driver: `      script:
        language: Shell
        runtime: base
`,
			wantError: "artifact must be selected when multiple artifacts are declared",
		},
		{
			name: "selection is not declared",
			artifacts: `    artifact:
      path: first.sh
`,
			driver: `      script:
        language: Shell
        runtime: base
        artifact: missing.sh
`,
			wantError: `artifact "missing.sh" is not declared`,
		},
		{
			name: "container rejects artifacts",
			artifacts: `    artifact:
      path: first.sh
`,
			driver: `      container:
        runtime: base
`,
			wantError: "container driver must not declare artifacts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for _, name := range []string{"first.sh", "second.sh"} {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, name),
					[]byte("#!/usr/bin/env bash\n"),
					0o600,
				))
			}

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
%s    driver:
      type: %s
%s`, testRuntimeA, tt.artifacts, map[bool]string{
				true:  "Container",
				false: "Script",
			}[strings.Contains(tt.driver, "container:")], tt.driver)
			planPath := filepath.Join(root, "certification-plan.yaml")
			require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

			_, err := CompileFile(planPath)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestCompileFileBundlesArtifactFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entrypoint := []byte("#!/bin/sh\nexit 0\n")
	script := []byte("#!/bin/bash\nprintf 'ok\\n'\n")

	require.NoError(t, os.WriteFile(filepath.Join(root, "entrypoint.sh"), entrypoint, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "check.sh"), script, 0o600))

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
      name: bundled-check.sh
      path: entrypoint.sh
      files:
      - name: source.sh
        path: check.sh
    driver:
      type: Script
      script:
        language: Shell
        runtime: base
`, testRuntimeA)
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	rendered, err := CompileFile(planPath)
	require.NoError(t, err)

	var test signalv1alpha1.CertificationTest
	require.NoError(t, yaml.Unmarshal(rendered, &test))
	require.Len(t, test.Spec.Artifacts, 1)

	parts := strings.Split(test.Spec.Artifacts[0].Content, artifactFilesMarker)
	require.Len(t, parts, 2)
	require.Equal(t, string(entrypoint), parts[0])

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	require.NoError(t, err)

	reader, err := gzip.NewReader(bytes.NewReader(decoded))
	require.NoError(t, err)
	decoded, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	var files []signalv1alpha1.TestArtifact
	require.NoError(t, json.Unmarshal(decoded, &files))

	sum := sha256.Sum256(script)
	require.Equal(t, []signalv1alpha1.TestArtifact{{
		Name:    "source.sh",
		Content: string(script),
		Digest:  "sha256:" + hex.EncodeToString(sum[:]),
	}}, files)
}

func TestCompileFileRejectsInvalidArtifactFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		files     string
		wantError string
	}{
		{
			name: "duplicate name",
			files: `      files:
      - name: source.sh
        path: first.sh
      - name: source.sh
        path: second.sh
`,
			wantError: `files[1].name "source.sh" is duplicated`,
		},
		{
			name: "path escape",
			files: `      files:
      - path: ../outside.sh
`,
			wantError: `files[0].path "../outside.sh" escapes spec.sourceRoot`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			root := filepath.Join(parent, "plan")
			require.NoError(t, os.Mkdir(root, 0o755))

			for _, name := range []string{"entrypoint.sh", "first.sh", "second.sh"} {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, name),
					[]byte("#!/bin/sh\n"),
					0o600,
				))
			}

			require.NoError(t, os.WriteFile(
				filepath.Join(parent, "outside.sh"),
				[]byte("#!/bin/sh\n"),
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
      path: entrypoint.sh
%s    driver:
      type: Script
      script:
        language: Shell
        runtime: base
`, testRuntimeA, tt.files)
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

func TestCompileTestsFileIncludesSourceWorkflowTemplate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "check.sh"),
		[]byte("#!/bin/sh\nprintf ok\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "workflows", "check.yaml"),
		[]byte(`apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: source-workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: example.invalid/check@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`),
		0o600,
	))

	plan := `apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: source-workflow
spec:
  owner: qualification-team
  version: source-123
  tests:
  - name: workflow-check
    displayName: Workflow check
    artifact:
      path: scripts/check.sh
    driver:
      type: WorkflowTemplate
      workflowTemplate:
        path: workflows/check.yaml
        artifact: check.sh
`
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	rendered, err := CompileTestsFile(planPath)
	require.NoError(t, err)

	documents := strings.Split(string(rendered), "---\n")
	require.Len(t, documents, 2)

	template := unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal([]byte(documents[0]), &template.Object))
	require.Equal(t, "source-workflow", template.GetName())

	test := signalv1alpha1.CertificationTest{}
	require.NoError(t, yaml.Unmarshal([]byte(documents[1]), &test))
	require.Equal(t, "source-workflow", test.Spec.Driver.WorkflowTemplate.Name)
	require.Equal(t, "check.sh", test.Spec.Driver.WorkflowTemplate.Artifact)

	digest, err := workflowTemplateSpecDigest(template)
	require.NoError(t, err)
	require.Equal(t, digest, test.Spec.Driver.WorkflowTemplate.Digest)
}

func TestCompileTestsFileRejectsInvalidSourceWorkflowTemplate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "scripts"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "scripts", "check.sh"),
		[]byte("#!/bin/sh\nprintf ok\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "workflows", "check.yaml"),
		[]byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: wrong-kind
`),
		0o600,
	))

	plan := `apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: source-workflow
spec:
  owner: qualification-team
  version: source-123
  tests:
  - name: workflow-check
    displayName: Workflow check
    artifact:
      path: scripts/check.sh
    driver:
      type: WorkflowTemplate
      workflowTemplate:
        path: workflows/check.yaml
        artifact: check.sh
`
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	_, err := CompileTestsFile(planPath)
	require.ErrorContains(
		t,
		err,
		"must contain an argoproj.io/v1alpha1 WorkflowTemplate",
	)
}

func TestCompileTestsFileRejectsInvalidSourceWorkflowTemplateReferences(t *testing.T) {
	t.Parallel()

	const validWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: source-workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: example.invalid/check@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
`

	tests := []struct {
		name           string
		workflow       string
		workflowSource string
		wantError      string
	}{
		{
			name: "namespace",
			workflow: strings.Replace(
				validWorkflow,
				"  name: source-workflow\n",
				"  name: source-workflow\n  namespace: project-signal\n",
				1,
			),
			workflowSource: "        path: workflows/check.yaml\n",
			wantError:      "must not set metadata.namespace",
		},
		{
			name:     "mixed path and identity",
			workflow: validWorkflow,
			workflowSource: `        path: workflows/check.yaml
        name: source-workflow
        digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`,
			wantError: "path is mutually exclusive with name and digest",
		},
		{
			name:           "path escape",
			workflow:       validWorkflow,
			workflowSource: "        path: ../outside.yaml\n",
			wantError:      `path "../outside.yaml" escapes spec.sourceRoot`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			root := filepath.Join(parent, "plan")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
			require.NoError(t, os.WriteFile(
				filepath.Join(root, "workflows", "check.yaml"),
				[]byte(tt.workflow),
				0o600,
			))
			require.NoError(t, os.WriteFile(
				filepath.Join(parent, "outside.yaml"),
				[]byte(tt.workflow),
				0o600,
			))

			plan := `apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: source-workflow
spec:
  owner: qualification-team
  version: source-123
  tests:
  - name: workflow-check
    displayName: Workflow check
    driver:
      type: WorkflowTemplate
      workflowTemplate:
` + tt.workflowSource
			planPath := filepath.Join(root, "certification-plan.yaml")
			require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

			_, err := CompileTestsFile(planPath)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestCompileTestsFileRejectsConflictingSourceWorkflowTemplates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "workflows"), 0o755))

	for name, command := range map[string]string{
		"first.yaml":  "true",
		"second.yaml": "false",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(root, "workflows", name),
			[]byte(fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: WorkflowTemplate
metadata:
  name: shared-workflow
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: example.invalid/check@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [%s]
`, command)),
			0o600,
		))
	}

	plan := `apiVersion: signal.unbounded-cloud.io/v1alpha1
kind: CertificationPlan
metadata:
  name: source-workflow
spec:
  owner: qualification-team
  version: source-123
  tests:
  - name: first-check
    displayName: First check
    driver:
      type: WorkflowTemplate
      workflowTemplate:
        path: workflows/first.yaml
  - name: second-check
    displayName: Second check
    driver:
      type: WorkflowTemplate
      workflowTemplate:
        path: workflows/second.yaml
`
	planPath := filepath.Join(root, "certification-plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(plan), 0o600))

	_, err := CompileTestsFile(planPath)
	require.ErrorContains(t, err, `WorkflowTemplate "shared-workflow" is defined with multiple specs`)
}

func mustCompileFile(t *testing.T, path string) []byte {
	t.Helper()

	rendered, err := CompileFile(path)
	require.NoError(t, err)

	return rendered
}

func mustCompileFileWithBindings(t *testing.T, path, bindingsPath string) []byte {
	t.Helper()

	rendered, err := CompileFileWithBindings(path, bindingsPath)
	require.NoError(t, err)

	return rendered
}
