// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package manifests

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const testImage = "registry.example.com/unbounded/unbounded-karpenter:v1.2.3"

func render(t *testing.T, options Options) []*unstructured.Unstructured {
	t.Helper()

	content, err := Render(options)
	if err != nil {
		t.Fatal(err)
	}

	var objects []*unstructured.Unstructured

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)

	for {
		object := new(unstructured.Unstructured)
		if err := decoder.Decode(object); err != nil {
			if err == io.EOF {
				return objects
			}

			t.Fatal(err)
		}

		if len(object.Object) != 0 {
			objects = append(objects, object)
		}
	}
}

func find(t *testing.T, objects []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()

	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}

	t.Fatalf("%s %s is missing", kind, name)

	return nil
}

func TestRendersThePinnedUpstreamCRDsAndNoNodeClassWrapper(t *testing.T) {
	objects := render(t, Options{Namespace: "unbounded-system", Image: testImage})

	for _, name := range []string{"nodepools.karpenter.sh", "nodeclaims.karpenter.sh"} {
		find(t, objects, "CustomResourceDefinition", name)
	}

	for _, object := range objects {
		if object.GetKind() == "CustomResourceDefinition" && strings.HasSuffix(object.GetName(), ".unbounded-cloud.io") {
			t.Fatalf("the plugin must not define %s: MachineClass is the NodeClass", object.GetName())
		}
	}
}

// TestCapacityAuthority asserts both the floor and the ceiling of what the
// plugin may do to the shared capacity resources.
func TestCapacityAuthority(t *testing.T) {
	objects := render(t, Options{Namespace: "unbounded-system", Image: testImage})
	role := find(t, objects, "ClusterRole", "unbounded-karpenter")

	rules, _, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil {
		t.Fatal(err)
	}

	granted := map[string][]string{}

	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("malformed RBAC rule")
		}

		groups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
		verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")

		if slices.Contains(groups, "*") || slices.Contains(resources, "*") || slices.Contains(verbs, "*") {
			t.Fatal("the plugin must not hold a wildcard rule")
		}

		if !slices.Contains(groups, "unbounded-cloud.io") {
			if slices.Contains(resources, "secrets") || slices.Contains(resources, "configmaps") {
				t.Fatal("the plugin must not read provider configuration or credentials")
			}

			continue
		}

		for _, resource := range resources {
			granted[resource] = append(granted[resource], verbs...)
		}
	}

	want := map[string][]string{
		"machineclasses":     {"get", "list", "watch"},
		"machineallocations": {"create", "delete", "get", "list", "patch", "watch"},
	}

	if len(granted) != len(want) {
		t.Fatalf("the plugin may touch only MachineClasses and MachineAllocations, got %v", granted)
	}

	for resource, verbs := range want {
		got := slices.Clone(granted[resource])
		slices.Sort(got)

		if !slices.Equal(got, verbs) {
			t.Fatalf("%s verbs are %v, want exactly %v", resource, got, verbs)
		}
	}
}

func TestAdmissionPolicyBindsThePluginToItsOwnAllocations(t *testing.T) {
	objects := render(t, Options{Namespace: "autoscaling", Image: testImage})
	policy := find(t, objects, "ValidatingAdmissionPolicy", "unbounded-karpenter-machineallocations")

	conditions, _, err := unstructured.NestedSlice(policy.Object, "spec", "matchConditions")
	if err != nil || len(conditions) != 1 {
		t.Fatal("the policy must match exactly the plugin's identity")
	}

	condition, _ := conditions[0].(map[string]any)
	// The match must not name the namespace: a namespace changed after
	// rendering would otherwise switch the policy off without any error.
	expression, _ := condition["expression"].(string)
	if !strings.Contains(expression, "endsWith(':unbounded-karpenter')") || strings.Contains(expression, "autoscaling") {
		t.Fatalf("the policy must match the plugin's service account in any namespace, got %q", expression)
	}

	validations, _, _ := unstructured.NestedSlice(policy.Object, "spec", "validations")

	if !slices.ContainsFunc(validations, func(validation any) bool {
		content, _ := validation.(map[string]any)
		rule, _ := content["expression"].(string)

		return strings.Contains(rule, "object.metadata.finalizers") && strings.Contains(rule, "oldObject.metadata.finalizers")
	}) {
		t.Fatal("the policy must keep the plugin from changing allocation finalizers")
	}

	rules, _, _ := unstructured.NestedSlice(policy.Object, "spec", "matchConstraints", "resourceRules")
	rule, _ := rules[0].(map[string]any)
	operations, _, _ := unstructured.NestedStringSlice(rule, "operations")
	slices.Sort(operations)

	if !slices.Equal(operations, []string{"CREATE", "DELETE", "UPDATE"}) {
		t.Fatalf("the policy must cover every write, got %v", operations)
	}

	if failure, _, _ := unstructured.NestedString(policy.Object, "spec", "failurePolicy"); failure != "Fail" {
		t.Fatal("the policy must fail closed")
	}

	binding := find(t, objects, "ValidatingAdmissionPolicyBinding", "unbounded-karpenter-machineallocations")

	actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if !slices.Equal(actions, []string{"Deny"}) {
		t.Fatalf("the policy must deny, got %v", actions)
	}
}

func TestDeployment(t *testing.T) {
	objects := render(t, Options{Namespace: "unbounded-system", Image: testImage, PriceCurrency: "EUR"})
	deployment := find(t, objects, "Deployment", "unbounded-karpenter")

	if replicas, _, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas"); replicas != 2 {
		t.Fatal("the controller runs leader-elected replicas")
	}

	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	container, _ := containers[0].(map[string]any)

	if container["image"] != testImage {
		t.Fatalf("unexpected image %v", container["image"])
	}

	var currency string

	environment, _, _ := unstructured.NestedSlice(container, "env")
	for _, raw := range environment {
		variable, _ := raw.(map[string]any)
		if variable["name"] == "UNBOUNDED_PRICE_CURRENCY" {
			currency, _ = variable["value"].(string)
		}
	}

	if currency != "EUR" {
		t.Fatalf("the price currency must reach the controller, got %q", currency)
	}

	account := find(t, objects, "ServiceAccount", "unbounded-karpenter")
	if len(account.GetAnnotations()) != 0 {
		t.Fatal("the provider-neutral plugin must not carry a cloud identity")
	}

	selector, found, _ := unstructured.NestedMap(deployment.Object, "spec", "template", "spec", "nodeSelector")
	if found && len(selector) != 0 {
		t.Fatal("the deployment must not assume one cloud's node labels")
	}
}

func TestRejectsUnsafeManifestValues(t *testing.T) {
	for name, options := range map[string]Options{
		"no image":          {Namespace: "unbounded-system"},
		"untagged image":    {Namespace: "unbounded-system", Image: "registry.example.com/unbounded/unbounded-karpenter"},
		"image with spaces": {Namespace: "unbounded-system", Image: "registry.example.com/a b:v1"},
		"image injection":   {Namespace: "unbounded-system", Image: "registry.example.com/a:v1\n  command: [sh]"},
		"invalid namespace": {Namespace: "Not_A_Namespace", Image: testImage},
		"invalid currency":  {Namespace: "unbounded-system", Image: testImage, PriceCurrency: "usd\""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(options); err == nil {
				t.Fatal("unsafe manifest values must be rejected")
			}
		})
	}
}

func TestDefaultsThePriceCurrency(t *testing.T) {
	content, err := Render(Options{Namespace: "unbounded-system", Image: testImage})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(content), `value: "USD"`) {
		t.Fatal("the price currency must default to USD")
	}
}
