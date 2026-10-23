// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestOwnPodAcknowledgmentRBAC(t *testing.T) {
	for _, namespace := range []string{"unbounded-system", "custom-system"} {
		t.Run(namespace, func(t *testing.T) {
			output := t.TempDir()
			if err := render.Render(".", output, map[string]string{"Namespace": namespace}); err != nil {
				t.Fatal(err)
			}

			raw, err := os.ReadFile(filepath.Join(output, "02-rbac.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
			found := false

			for {
				var role rbacv1.Role
				if err := decoder.Decode(&role); err != nil {
					if err == io.EOF {
						break
					}

					t.Fatal(err)
				}

				if role.Kind != "Role" || role.Namespace != namespace {
					continue
				}

				for _, rule := range role.Rules {
					if slices.Equal(rule.APIGroups, []string{""}) &&
						slices.Equal(rule.Resources, []string{"pods"}) {
						if !slices.Equal(rule.Verbs, []string{"get", "patch"}) || len(rule.ResourceNames) != 0 {
							t.Fatalf("unexpected own-Pod permission: %+v", rule)
						}

						found = true
					}
				}
			}

			if !found {
				t.Fatal("missing namespaced own-Pod get/patch permission")
			}
		})
	}
}

func TestNodePodIdentityEnvironment(t *testing.T) {
	output := t.TempDir()
	if err := render.Render(".", output, nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(output, "03-daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var daemonSet appsv1.DaemonSet

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for daemonSet.Kind != "DaemonSet" {
		if err := decoder.Decode(&daemonSet); err != nil {
			t.Fatal(err)
		}
	}

	if len(daemonSet.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(daemonSet.Spec.Template.Spec.Containers))
	}

	fields := map[string]string{}

	for _, env := range daemonSet.Spec.Template.Spec.Containers[0].Env {
		if env.ValueFrom != nil && env.ValueFrom.FieldRef != nil {
			fields[env.Name] = env.ValueFrom.FieldRef.FieldPath
		}
	}

	for name, field := range map[string]string{
		"NODE_NAME":     "spec.nodeName",
		"POD_NAMESPACE": "metadata.namespace",
		"POD_NAME":      "metadata.name",
		"POD_UID":       "metadata.uid",
	} {
		if fields[name] != field {
			t.Fatalf("%s fieldRef = %q, want %q", name, fields[name], field)
		}
	}
}
