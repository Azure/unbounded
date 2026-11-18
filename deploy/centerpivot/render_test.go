// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package centerpivot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// TestRenderedRBAC holds the two ClusterRoles to what each account needs. The
// admission policies narrow the Node and Machine writes further.
func TestRenderedRBAC(t *testing.T) {
	output := t.TempDir()
	if err := render.Render(".", output, map[string]string{"Namespace": "operator-ns"}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(output, "01-rbac.yaml"))
	if err != nil {
		t.Fatalf("read rendered RBAC: %v", err)
	}

	roles := map[string][]rbacv1.PolicyRule{}

	for _, document := range strings.Split(string(data), "\n---") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatalf("parse RBAC document: %v", err)
		}

		if role.Kind == "ClusterRole" {
			roles[role.Name] = role.Rules
		}
	}

	want := map[string][]rbacv1.PolicyRule{
		"centerpivot": {
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{"unbounded-cloud.io"}, Resources: []string{"machines"}, Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{"unbounded-cloud.io"}, Resources: []string{"sites", "machineoperations"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"pods/eviction"}, Verbs: []string{"create"}},
			{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "deployments", "replicasets", "statefulsets"}, Verbs: []string{"get"}},
			{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		},
		"centerpivot-node-info": {
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "patch"}},
		},
	}

	for name, rules := range want {
		got, err := yaml.Marshal(roles[name])
		if err != nil {
			t.Fatal(err)
		}

		expected, err := yaml.Marshal(rules)
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != string(expected) {
			t.Errorf("ClusterRole %s rules:\n%s\nwant:\n%s", name, got, expected)
		}
	}
}
