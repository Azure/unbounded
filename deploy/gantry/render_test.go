// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// TestGantryManifestsRender renders the gantry templates and decodes every
// document through the same YAML decoder the kubectl plugin uses, so a
// malformed template or unresolved parameter fails the default test suite.
func TestGantryManifestsRender(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	templatesDir := filepath.Join(repoRoot, "deploy", "gantry")

	renderDir := t.TempDir()
	require.NoError(t, render.Render(templatesDir, renderDir, map[string]string{
		"Namespace": "custom-ns",
		"Image":     "ghcr.io/azure/gantry:v1.2.3",
	}))

	kinds := map[string]int{}

	var (
		namespaceObjectName string
		image               string
	)

	for _, name := range []string{"serviceaccount.yaml", "configmap.yaml", "daemonset.yaml"} {
		data, err := os.ReadFile(filepath.Join(renderDir, name))
		require.NoError(t, err, name)
		require.NotContains(t, string(data), "{{", "unresolved template directive in %s", name)

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}

			err := decoder.Decode(obj)
			if err == io.EOF {
				break
			}

			require.NoError(t, err, "decode %s", name)

			if len(obj.Object) == 0 {
				continue
			}

			kind := obj.GetKind()
			kinds[kind]++

			switch kind {
			case "Namespace":
				namespaceObjectName = obj.GetName()
			case "ConfigMap", "ServiceAccount", "Role", "RoleBinding", "DaemonSet":
				require.Equal(t, "custom-ns", obj.GetNamespace(), "%s namespace", kind)
			}

			if kind == "DaemonSet" {
				containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
				require.NoError(t, err)

				for _, c := range containers {
					cm, ok := c.(map[string]any)
					require.True(t, ok)

					if cm["name"] == "gantry" {
						image, _ = cm["image"].(string)
					}
				}
			}
		}
	}

	require.Equal(t, 1, kinds["Namespace"])
	require.Equal(t, 1, kinds["ServiceAccount"])
	require.Equal(t, 1, kinds["ConfigMap"])
	require.Equal(t, 1, kinds["DaemonSet"])
	require.Equal(t, 1, kinds["ClusterRole"])
	require.Equal(t, 1, kinds["ClusterRoleBinding"])
	require.Equal(t, 1, kinds["Role"])
	require.Equal(t, 1, kinds["RoleBinding"])
	require.Equal(t, 1, kinds["PriorityClass"])

	require.Equal(t, "custom-ns", namespaceObjectName)
	require.Equal(t, "ghcr.io/azure/gantry:v1.2.3", image)
}
