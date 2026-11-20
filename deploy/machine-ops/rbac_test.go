// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestControllerCanResolveMachineConfigurationVersion(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("02-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read RBAC template: %v", err)
	}

	document := strings.SplitN(string(data), "\n---", 2)[0]
	document = strings.Replace(document, `{{- $controllerName := default "azure-machine-ops-controller" .ControllerName }}`, "", 1)
	document = strings.ReplaceAll(document, "{{ $controllerName }}", "azure-machine-ops-controller")

	var clusterRole rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(document), &clusterRole); err != nil {
		t.Fatalf("parse controller ClusterRole: %v", err)
	}

	for _, verb := range []string{"get", "list"} {
		if !grants(clusterRole.Rules, "unbounded-cloud.io", "machineconfigurationversions", verb) {
			t.Fatalf("controller ClusterRole does not grant %s on machineconfigurationversions", verb)
		}
	}
}

func TestKubeSystemBootstrapRoleLimitsServicesToKubeDNS(t *testing.T) {
	t.Parallel()

	role := kubeSystemBootstrapRole(t)

	services := false

	for _, rule := range role.Rules {
		if !slices.Contains(rule.Resources, "services") {
			continue
		}

		services = true

		if !slices.Equal(rule.Verbs, []string{"get"}) {
			t.Fatalf("services rule grants %v, want only [get]", rule.Verbs)
		}

		if !slices.Equal(rule.ResourceNames, []string{"kube-dns"}) {
			t.Fatalf("services rule is limited to %v, want only [kube-dns]", rule.ResourceNames)
		}
	}

	if !services {
		t.Fatal("kube-system Role grants nothing on services")
	}

	// Bootstrap token Secret names come from Machine specs, so the secrets
	// grant must keep working for any name.
	if !grants(role.Rules, "", "secrets", "get") {
		t.Fatal("kube-system Role does not grant get on secrets")
	}
}

// kubeSystemBootstrapRole parses the Role that 02-rbac.yaml.tmpl defines in
// kube-system, with template directives resolved to their default values.
func kubeSystemBootstrapRole(t *testing.T) *rbacv1.Role {
	t.Helper()

	data, err := os.ReadFile("02-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read RBAC template: %v", err)
	}

	content := strings.Replace(string(data), `{{- $controllerName := default "azure-machine-ops-controller" .ControllerName }}`, "", 1)
	content = strings.ReplaceAll(content, "{{ $controllerName }}", "azure-machine-ops-controller")
	content = strings.ReplaceAll(content, `{{ default "unbounded-system" .Namespace }}`, "unbounded-system")

	for document := range strings.SplitSeq(content, "\n---") {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &head); err != nil {
			t.Fatalf("parse document: %v", err)
		}

		if head.Kind != "Role" || head.Metadata.Namespace != "kube-system" {
			continue
		}

		var role rbacv1.Role
		if err := yaml.UnmarshalStrict([]byte(document), &role); err != nil {
			t.Fatalf("parse kube-system Role: %v", err)
		}

		return &role
	}

	t.Fatal("kube-system Role not found in RBAC template")

	return nil
}

func grants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) &&
			slices.Contains(rule.Resources, resource) &&
			slices.Contains(rule.Verbs, verb) {
			return true
		}
	}

	return false
}
