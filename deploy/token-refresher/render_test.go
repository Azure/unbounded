// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tokenrefresher

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestRenderedRBACIsLeastPrivilege(t *testing.T) {
	output := t.TempDir()
	if err := render.Render(".", output, map[string]string{"Namespace": "operator-ns"}); err != nil {
		t.Fatalf("render manifests: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(output, "01-rbac.yaml"))
	if err != nil {
		t.Fatalf("read rendered RBAC: %v", err)
	}

	docs := splitYAMLDocuments(string(data))
	if len(docs) != 7 {
		t.Fatalf("RBAC contains %d documents, want 7", len(docs))
	}

	assertRoleRules(t, docs, "ClusterRole", "", "unbounded-cloud.io", "sites", []string{"get", "list", "watch"})
	assertRoleRules(t, docs, "ClusterRole", "", "unbounded-cloud.io", "machines", []string{"get", "list", "watch", "patch"})
	assertRoleRules(t, docs, "Role", "kube-system", "", "secrets", []string{"get", "list", "watch", "create", "patch", "update"})
	assertRoleRules(t, docs, "Role", "operator-ns", "coordination.k8s.io", "leases", []string{"get", "list", "watch", "create", "patch", "update"})
}

func splitYAMLDocuments(data string) []string {
	var documents []string
	for _, document := range strings.Split(data, "\n---") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}

	return documents
}

func assertRoleRules(t *testing.T, documents []string, kind, namespace, group, resource string, verbs []string) {
	t.Helper()

	for _, document := range documents {
		var metadata struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(document), &metadata); err != nil {
			t.Fatalf("parse RBAC document: %v", err)
		}
		if metadata.Kind != kind || metadata.Metadata.Name != "token-refresher" || metadata.Metadata.Namespace != namespace {
			continue
		}
		for _, rule := range metadata.Rules {
			if slices.Equal(rule.APIGroups, []string{group}) && slices.Equal(rule.Resources, []string{resource}) && slices.Equal(rule.Verbs, verbs) {
				return
			}
		}

		t.Fatalf("%s/%s has no matching rule for %s/%s with verbs %v: %+v", kind, namespace, group, resource, verbs, metadata.Rules)
	}

	t.Fatalf("%s token-refresher in namespace %q not found", kind, namespace)
}
