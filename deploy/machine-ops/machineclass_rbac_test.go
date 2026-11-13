// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestMachineClassRBACGrantsExactly(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("04-machineclass-rbac.yaml.tmpl")
	if err != nil {
		t.Fatalf("read MachineClass RBAC template: %v", err)
	}

	if strings.Contains(string(data), "{{") || strings.Contains(string(data), "}}") {
		t.Fatal("MachineClass RBAC template contains a template directive")
	}

	want := map[string][]string{
		"unbounded-machineclass-controller": {
			"unbounded-cloud.io/machineclasses:get",
			"unbounded-cloud.io/machineclasses:list",
			"unbounded-cloud.io/machineclasses:watch",
			"unbounded-cloud.io/machineclasses/status:get",
			"unbounded-cloud.io/machineclasses/status:update",
			"unbounded-cloud.io/machineclasses/status:patch",
			"unbounded-cloud.io/sites:get",
			"unbounded-cloud.io/sites:list",
			"unbounded-cloud.io/sites:watch",
			"unbounded-cloud.io/machineconfigurationversions:get",
			"unbounded-cloud.io/machineconfigurationversions:list",
			"unbounded-cloud.io/machineconfigurationversions:watch",
			"unbounded-cloud.io/machineoperationcredentials:get",
			"unbounded-cloud.io/machineoperationcredentials:list",
			"unbounded-cloud.io/machineoperationcredentials:watch",
		},
		"unbounded-machineclass-controller-provider-config": {
			"/configmaps:get",
			"/configmaps:list",
			"/configmaps:watch",
		},
		"unbounded-machineclass-controller-credentials": {
			"/secrets:get",
		},
	}

	got := map[string][]string{}

	for _, document := range strings.Split(string(data), "\n---") {
		var typeMeta struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(document), &typeMeta); err != nil {
			t.Fatalf("parse document: %v", err)
		}

		if typeMeta.Kind == "" {
			continue
		}

		if typeMeta.APIVersion != rbacv1.SchemeGroupVersion.String() || typeMeta.Kind != "ClusterRole" {
			t.Fatalf("unexpected %s %s; the template must hold only ClusterRoles", typeMeta.APIVersion, typeMeta.Kind)
		}

		var clusterRole rbacv1.ClusterRole
		if err := yaml.UnmarshalStrict([]byte(document), &clusterRole); err != nil {
			t.Fatalf("parse ClusterRole: %v", err)
		}

		if _, exists := got[clusterRole.Name]; exists {
			t.Fatalf("ClusterRole %s appears more than once", clusterRole.Name)
		}

		if clusterRole.AggregationRule != nil {
			t.Fatalf("ClusterRole %s must not aggregate other roles", clusterRole.Name)
		}

		got[clusterRole.Name] = grantSet(t, clusterRole)
	}

	if len(got) != len(want) {
		t.Fatalf("got ClusterRoles %v, want exactly %d", slices.Sorted(maps.Keys(got)), len(want))
	}

	for name, grants := range want {
		slices.Sort(grants)

		if !slices.Equal(got[name], grants) {
			t.Fatalf("ClusterRole %s grants %v, want exactly %v", name, got[name], grants)
		}
	}
}

// grantSet flattens a ClusterRole's rules into sorted, unique
// "group/resource:verb" grants, and fails on wildcards, resource names and
// non-resource URLs.
func grantSet(t *testing.T, clusterRole rbacv1.ClusterRole) []string {
	t.Helper()

	var grants []string

	for _, rule := range clusterRole.Rules {
		if len(rule.NonResourceURLs) > 0 || len(rule.ResourceNames) > 0 {
			t.Fatalf("ClusterRole %s has a rule with non-resource URLs or resource names", clusterRole.Name)
		}

		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					if group == "*" || resource == "*" || verb == "*" || strings.Contains(resource, "*") {
						t.Fatalf("ClusterRole %s grants a wildcard", clusterRole.Name)
					}

					grants = append(grants, group+"/"+resource+":"+verb)
				}
			}
		}
	}

	slices.Sort(grants)

	return slices.Compact(grants)
}
