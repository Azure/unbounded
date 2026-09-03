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
	document = strings.Replace(document, `{{- $controllerName := default "machine-ops-controller" .ControllerName }}`, "", 1)
	document = strings.ReplaceAll(document, "{{ $controllerName }}", "machine-ops-controller")

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
