// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolvePublishInputsWorkflowDispatchUsesExplicitInputs(t *testing.T) {
	output := filepath.Join(t.TempDir(), "github-output")

	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "v0.4.0-alpha")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "1.34.9, v1.35.6")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", "")

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "v0.4.0-alpha", values["tag"])
	requireJSONEqual(t, []string{"v1.34.9", "v1.35.6"}, values["kubernetes_versions"])
	requireJSONEqual(t, []kubernetesVersionGroup{
		{Minor: "1.34", Versions: []string{"v1.34.9"}},
		{Minor: "1.35", Versions: []string{"v1.35.6"}},
	}, values["kubernetes_version_groups"])
}

func TestResolvePublishInputsWorkflowDispatchDefaultsTagAndVersions(t *testing.T) {
	dir := t.TempDir()
	versionsFile := filepath.Join(dir, "versions.txt")
	require.NoError(t, os.WriteFile(versionsFile, []byte(`
# current supported patches
1.34.8
v1.34.9
`), 0o644))

	output := filepath.Join(dir, "github-output")

	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", versionsFile)

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "0123456789ab", values["tag"])
	requireJSONEqual(t, []string{"v1.34.8", "v1.34.9"}, values["kubernetes_versions"])
	requireJSONEqual(t, []kubernetesVersionGroup{
		{Minor: "1.34", Versions: []string{"v1.34.8", "v1.34.9"}},
	}, values["kubernetes_version_groups"])
}

func TestDefaultKubernetesVersionsIncludes136(t *testing.T) {
	versions, err := defaultKubernetesVersions()
	require.NoError(t, err)

	require.Subset(t, normalizeKubernetesVersions(versions), []string{
		"v1.36.0",
		"v1.36.1",
		"v1.36.2",
	})
}

func TestResolvePublishInputsTagPushUsesRefTagAndDefaultVersions(t *testing.T) {
	dir := t.TempDir()
	versionsFile := filepath.Join(dir, "versions.txt")
	require.NoError(t, os.WriteFile(versionsFile, []byte("v1.35.5\nv1.35.6\n"), 0o644))

	output := filepath.Join(dir, "github-output")

	t.Setenv("EVENT_NAME", "push")
	t.Setenv("REF_NAME", "agent-artifacts/alpha-test")
	t.Setenv("INPUT_TAG", "ignored")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "v1.34.9")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", versionsFile)

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "alpha-test", values["tag"])
	requireJSONEqual(t, []string{"v1.35.5", "v1.35.6"}, values["kubernetes_versions"])
}

func TestResolvePublishInputsRequiresTag(t *testing.T) {
	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "v1.34.9")
	t.Setenv("GITHUB_SHA_VALUE", "")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "github-output"))
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", "")

	require.ErrorContains(t, resolvePublishInputs(), "artifact tag could not be resolved")
}

func TestNormalizeKubernetesVersions(t *testing.T) {
	got := normalizeKubernetesVersions("1.34.9, v1.35.0\n1.35.1")
	require.Equal(t, []string{"v1.34.9", "v1.35.0", "v1.35.1"}, got)
}

func TestGroupKubernetesVersionsByMinor(t *testing.T) {
	got, err := groupKubernetesVersionsByMinor([]string{"v1.34.8", "v1.34.9", "v1.35.0"})
	require.NoError(t, err)
	require.Equal(t, []kubernetesVersionGroup{
		{Minor: "1.34", Versions: []string{"v1.34.8", "v1.34.9"}},
		{Minor: "1.35", Versions: []string{"v1.35.0"}},
	}, got)
}

func TestGroupKubernetesVersionsByMinorRejectsInvalidSemver(t *testing.T) {
	_, err := groupKubernetesVersionsByMinor([]string{"not-a-version"})
	require.ErrorContains(t, err, "parse Kubernetes version")
}

func TestKubernetesVersionsFromJSON(t *testing.T) {
	versions, err := kubernetesVersionsFromJSON(`["v1.34.9","v1.35.6"]`)
	require.NoError(t, err)
	require.Equal(t, []string{"v1.34.9", "v1.35.6"}, versions)

	_, err = kubernetesVersionsFromJSON(`[]`)
	require.ErrorContains(t, err, "must contain at least one version")
}

func readGitHubOutput(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	values := map[string]string{}

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		require.Truef(t, ok, "invalid GitHub output line %q", line)

		values[key] = value
	}

	return values
}

func requireJSONEqual[T any](t *testing.T, expected T, raw string) {
	t.Helper()

	var got T
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, expected, got)
}
