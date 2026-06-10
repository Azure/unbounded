// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machina

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestMachinaControllerCSRPermissions(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	require.NoError(t, render.Render(".", outputDir, map[string]string{
		"Namespace":       "unbounded-kube",
		"ControllerImage": "ghcr.io/example/machina:test",
	}))

	raw, err := os.ReadFile(filepath.Join(outputDir, "02-rbac.yaml"))
	require.NoError(t, err)

	var clusterRole map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)
		if doc["kind"] != "ClusterRole" {
			continue
		}

		metadata, _ := doc["metadata"].(map[string]any)
		if metadata["name"] == "machina-controller" {
			clusterRole = doc
			break
		}
	}

	require.NotNil(t, clusterRole)
	rule := findRule(t, clusterRole, "certificates.k8s.io", "certificatesigningrequests")
	require.ElementsMatch(t, []string{"get", "list", "watch", "update"}, stringSlice(t, rule["verbs"]))
}

func findRule(t *testing.T, clusterRole map[string]any, apiGroup, resource string) map[string]any {
	t.Helper()

	rules, ok := clusterRole["rules"].([]any)
	require.True(t, ok)

	for _, item := range rules {
		rule, ok := item.(map[string]any)
		require.True(t, ok)

		if containsString(t, rule["apiGroups"], apiGroup) && containsString(t, rule["resources"], resource) {
			return rule
		}
	}

	require.Failf(t, "rule not found", "apiGroup=%s resource=%s", apiGroup, resource)
	return nil
}

func containsString(t *testing.T, value any, want string) bool {
	t.Helper()

	for _, item := range stringSlice(t, value) {
		if item == want {
			return true
		}
	}

	return false
}

func stringSlice(t *testing.T, value any) []string {
	t.Helper()

	items, ok := value.([]any)
	require.True(t, ok)

	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		require.True(t, ok)
		out = append(out, s)
	}

	return out
}
