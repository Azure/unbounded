// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	runtimeutil "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

// kindToResource maps the Kinds the reaper deletes (reapableKinds) to their
// RBAC resource name. Keep in sync with reapableKinds(): a new reaped kind must
// be added here (and granted deletecollection in the operator ClusterRole),
// which TestOperatorClusterRoleGrantsReaperDeletes enforces.
var kindToResource = map[string]string{
	"Deployment":     "deployments",
	"DaemonSet":      "daemonsets",
	"Service":        "services",
	"ConfigMap":      "configmaps",
	"Secret":         "secrets",
	"ServiceAccount": "serviceaccounts",
	"Role":           "roles",
	"RoleBinding":    "rolebindings",
}

// loadOperatorClusterRole parses the operator ClusterRole from its template. The
// ClusterRole document contains no Go-template actions (only the accompanying
// ClusterRoleBinding does), so it parses as plain YAML directly from the source
// of truth without needing the manifests to be rendered.
func loadOperatorClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}

	path := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"deploy", "unbounded-operator", "02-rbac.yaml.tmpl")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read operator rbac template: %v", err)
	}

	for _, doc := range strings.Split(string(data), "\n---") {
		doc = strings.TrimSpace(doc)
		// Skip empty docs and the templated ClusterRoleBinding.
		if doc == "" || strings.Contains(doc, "{{") {
			continue
		}

		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}

		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			continue
		}

		if meta.Kind == "ClusterRole" && meta.Metadata.Name == "unbounded-operator" {
			var cr rbacv1.ClusterRole
			if err := yaml.Unmarshal([]byte(doc), &cr); err != nil {
				t.Fatalf("parse operator ClusterRole: %v", err)
			}

			return &cr
		}
	}

	t.Fatalf("operator ClusterRole not found in %s", path)

	return nil
}

func clusterRoleGrants(cr *rbacv1.ClusterRole, group, resource, verb string) bool {
	for _, rule := range cr.Rules {
		if contains(rule.APIGroups, group) &&
			contains(rule.Resources, resource) &&
			(contains(rule.Verbs, verb) || contains(rule.Verbs, "*")) {
			return true
		}
	}

	return false
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}

	return false
}

// TestOperatorClusterRoleAllowsComponentRBACInstall guards against the
// privilege-escalation failure the faithful upgrade e2e surfaced: the operator
// installs the net/machina component RBAC, whose Roles/ClusterRoles grant
// permissions the operator does not itself hold, so the apiserver requires the
// operator to hold escalate + bind on roles/clusterroles.
func TestOperatorClusterRoleAllowsComponentRBACInstall(t *testing.T) {
	cr := loadOperatorClusterRole(t)

	for _, resource := range []string{"roles", "clusterroles"} {
		for _, verb := range []string{"escalate", "bind"} {
			if !clusterRoleGrants(cr, "rbac.authorization.k8s.io", resource, verb) {
				t.Fatalf("operator ClusterRole must grant %q on %q (needed to install component RBAC without privilege escalation)", verb, resource)
			}
		}
	}
}

// TestOperatorClusterRoleGrantsReaperDeletes guards against the reaper's
// DeleteAllOf (deletecollection) being forbidden: the operator ClusterRole must
// grant deletecollection on every kind the reaper deletes by label.
func TestOperatorClusterRoleGrantsReaperDeletes(t *testing.T) {
	cr := loadOperatorClusterRole(t)

	scheme := runtimeutil.NewScheme()
	for _, add := range []func(*runtimeutil.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		rbacv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	for _, obj := range reapableKinds() {
		gvks, _, err := scheme.ObjectKinds(obj)
		if err != nil || len(gvks) == 0 {
			t.Fatalf("resolve GVK for %T: %v", obj, err)
		}

		gvk := gvks[0]

		resource, ok := kindToResource[gvk.Kind]
		if !ok {
			t.Fatalf("no resource mapping for reaped kind %q; add it to kindToResource and grant deletecollection in the operator ClusterRole", gvk.Kind)
		}

		if !clusterRoleGrants(cr, gvk.Group, resource, "deletecollection") {
			t.Fatalf("operator ClusterRole must grant deletecollection on %q (apiGroup %q) for the reaper's DeleteAllOf", resource, gvk.Group)
		}
	}
}

func TestOperatorClusterRoleAllowsGantryPriorityClass(t *testing.T) {
	cr := loadOperatorClusterRole(t)

	for _, verb := range []string{"create", "patch", "update"} {
		if !clusterRoleGrants(cr, "scheduling.k8s.io", "priorityclasses", verb) {
			t.Fatalf("operator ClusterRole must grant %q on priorityclasses (needed to install the gantry PriorityClass)", verb)
		}
	}

	for _, verb := range []string{"delete", "deletecollection"} {
		if clusterRoleGrants(cr, "scheduling.k8s.io", "priorityclasses", verb) {
			t.Fatalf("operator ClusterRole must not grant %q on priorityclasses", verb)
		}
	}
}

func TestOperatorClusterRoleGrantsForeignWorkloadAudit(t *testing.T) {
	cr := loadOperatorClusterRole(t)
	resources := []struct {
		group    string
		resource string
	}{
		{group: "", resource: "pods"},
		{group: "", resource: "persistentvolumeclaims"},
		{group: "", resource: "replicationcontrollers"},
		{group: "apps", resource: "deployments"},
		{group: "apps", resource: "replicasets"},
		{group: "apps", resource: "daemonsets"},
		{group: "apps", resource: "statefulsets"},
		{group: "batch", resource: "jobs"},
		{group: "batch", resource: "cronjobs"},
	}

	for _, audited := range resources {
		for _, verb := range []string{"get", "list", "watch"} {
			if !clusterRoleGrants(cr, audited.group, audited.resource, verb) {
				t.Fatalf("operator ClusterRole must grant %s on %q (apiGroup %q) for the foreign workload audit", verb, audited.resource, audited.group)
			}
		}
	}
}

// loadMachinaRBAC parses every RBAC document out of the rendered machina
// manifests. Unlike the operator ClusterRole, the machina RBAC template carries
// Go-template actions in its metadata, so it is read from the rendered output
// that the operator actually embeds and applies. Relies on `make test`
// rendering the manifests first.
func loadMachinaRBAC(t *testing.T) (roles []rbacv1.Role, clusterRoles []rbacv1.ClusterRole) {
	t.Helper()

	files, err := component.YamlFiles(machinamanifests.Manifests)
	if err != nil {
		t.Fatalf("list machina manifests: %v", err)
	}

	for _, file := range files {
		data, err := fs.ReadFile(machinamanifests.Manifests, file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for _, doc := range strings.Split(string(data), "\n---") {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			var meta struct {
				Kind string `json:"kind"`
			}

			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				continue
			}

			switch meta.Kind {
			case "Role":
				var role rbacv1.Role
				if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
					t.Fatalf("parse Role in %s: %v", file, err)
				}

				roles = append(roles, role)
			case "ClusterRole":
				var clusterRole rbacv1.ClusterRole
				if err := yaml.Unmarshal([]byte(doc), &clusterRole); err != nil {
					t.Fatalf("parse ClusterRole in %s: %v", file, err)
				}

				clusterRoles = append(clusterRoles, clusterRole)
			}
		}
	}

	return roles, clusterRoles
}

// TestMachinaControllerRoleGrantsNoConfigMapAccess guards the security boundary
// for operator-owned ConfigMaps in the install namespace, including the
// component workload overrides ConfigMap.
//
// The machina controller reads exactly one ConfigMap, kube-root-ca.crt in
// kube-public (cmd/machina/machina/controller/cluster_info.go), through the
// uncached clientset; that read is served by the cluster-wide configmaps get in
// its ClusterRole. machina-config reaches the pod as a volume mount, which
// needs no RBAC, and leader election uses Leases. No machina controller watches
// ConfigMaps and machinaCacheOptions() scopes only Secrets, so no informer is
// established either.
//
// A namespaced ConfigMap grant would therefore be unused, and would let a
// compromised machina controller write ConfigMaps the operator owns and acts
// on. RBAC cannot scope create by resourceNames, so the grant has to be absent
// rather than narrowed.
func TestMachinaControllerRoleGrantsNoConfigMapAccess(t *testing.T) {
	roles, _ := loadMachinaRBAC(t)

	var checked int

	for _, role := range roles {
		if role.Name != "machina-controller" {
			continue
		}

		checked++

		for _, rule := range role.Rules {
			if !contains(rule.APIGroups, "") {
				continue
			}

			if contains(rule.Resources, "configmaps") || contains(rule.Resources, "*") {
				t.Errorf("machina-controller Role in namespace %q grants configmaps %v; the controller does not use it and it would expose operator-owned ConfigMaps",
					role.Namespace, rule.Verbs)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no machina-controller Role found in the rendered machina manifests")
	}
}

// TestMachinaControllerClusterRoleKeepsClusterInfoRead is the other half of
// TestMachinaControllerRoleGrantsNoConfigMapAccess: removing the namespaced
// grant is only safe while the cluster-wide read that cluster_info.go depends
// on survives. If this fails, machina cannot resolve kube-root-ca.crt and
// bootstrap script generation breaks at runtime rather than at build time.
func TestMachinaControllerClusterRoleKeepsClusterInfoRead(t *testing.T) {
	_, clusterRoles := loadMachinaRBAC(t)

	for _, clusterRole := range clusterRoles {
		if clusterRole.Name != "machina-controller" {
			continue
		}

		if clusterRoleGrants(&clusterRole, "", "configmaps", "get") {
			return
		}

		t.Fatal("machina-controller ClusterRole must grant configmaps get for the kube-root-ca.crt read in cluster_info.go")
	}

	t.Fatal("no machina-controller ClusterRole found in the rendered machina manifests")
}
