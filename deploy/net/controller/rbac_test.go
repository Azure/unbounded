// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestBoundObjectCacheRBAC(t *testing.T) {
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

				for _, rule := range role.Rules {
					if slices.Contains(rule.Resources, "serviceaccounts") || slices.Contains(rule.Resources, "*") {
						if role.Kind != "Role" || role.Namespace != namespace ||
							!slices.Equal(rule.APIGroups, []string{""}) ||
							!slices.Equal(rule.Verbs, []string{"list", "watch"}) ||
							len(rule.ResourceNames) != 0 {
							t.Fatalf("unexpected service account permission: %+v %+v", role.ObjectMeta, rule)
						}

						found = true
					}
				}

				if role.Kind == "Role" && role.Namespace == namespace {
					for _, verb := range []string{"get", "list", "watch"} {
						if !grantsPodRead(role.Rules, verb) {
							t.Fatalf("missing Pod %s permission in controller namespace", verb)
						}
					}
				}
			}

			if !found {
				t.Fatal("missing namespaced service account list/watch permissions")
			}
		})
	}
}

func grantsPodRead(rules []rbacv1.PolicyRule, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "pods") && slices.Contains(rule.Verbs, verb) {
			return true
		}
	}

	return false
}
