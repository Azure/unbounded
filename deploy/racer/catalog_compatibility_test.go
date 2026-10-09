// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestCatalogCRDPackaging(t *testing.T) {
	for _, resource := range []string{"clustercaches", "clustervolumes"} {
		name := "racer.unbounded-cloud.io_" + resource + ".yaml"
		generated, err := os.ReadFile("../../api/racer/v1alpha1/crd/" + name)
		require.NoError(t, err)
		packaged, err := fs.ReadFile(manifests.Manifests, "crd/"+name)
		require.NoError(t, err)
		require.Equal(t, string(generated), string(packaged))

		standalone, err := os.ReadFile("rendered/crd/" + name)
		require.NoError(t, err, "run make racer-manifests")
		require.Equal(t, string(generated), string(standalone))
	}
}

func TestCatalogRBACReadOnlyForControllerAndOperator(t *testing.T) {
	for _, tt := range []struct{ directory, file, role string }{
		{".", "rbac.yaml", "racer-controller"},
		{"../unbounded-operator", "02-rbac.yaml", "unbounded-operator"},
	} {
		t.Run(tt.role, func(t *testing.T) {
			out := t.TempDir()
			require.NoError(t, render.Render(tt.directory, out, nil))

			env := &component.Env{Namespace: "unbounded-system"}
			objects, err := env.DecodeManifestFiles(os.DirFS(out), []string{tt.file}, nil)
			require.NoError(t, err)

			found := false

			for _, obj := range objects {
				if obj.GetKind() != "ClusterRole" || obj.GetName() != tt.role {
					continue
				}

				found = true
				role := &rbacv1.ClusterRole{}
				require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, role))

				for _, resource := range []string{"clustercaches", "clustervolumes"} {
					verbs := map[string]bool{}

					for _, rule := range role.Rules {
						for _, group := range rule.APIGroups {
							for _, granted := range rule.Resources {
								if (group == "racer.unbounded-cloud.io" || group == "*") && (granted == resource || granted == "*") {
									require.Empty(t, rule.ResourceNames)

									for _, verb := range rule.Verbs {
										verbs[verb] = true
									}
								}
							}
						}
					}

					require.Equal(t, map[string]bool{"get": true, "list": true, "watch": true}, verbs, resource)
				}
			}

			require.True(t, found)
		})
	}
}
