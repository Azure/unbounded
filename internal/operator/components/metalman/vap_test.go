// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"testing"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	bootstrapPolicyName = "metalman-bootstrap-machine-restriction"
	bootstrapSAUsername = "system:serviceaccount:unbounded-system:metalman-bootstrap"
	credentialIDExtra   = "authentication.kubernetes.io/credential-id"
)

func supportObject(t *testing.T, kind, name string) *unstructured.Unstructured {
	t.Helper()

	env := &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: component.DefaultNamespace,
	}

	objs, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, obj := range objs {
		if obj.GetKind() == kind && obj.GetName() == name {
			return obj
		}
	}

	t.Fatalf("support objects do not contain %s/%s", kind, name)

	return nil
}

func TestBootstrapPolicyBinding(t *testing.T) {
	binding := supportObject(t, "ValidatingAdmissionPolicyBinding", bootstrapPolicyName)

	policyName, _, _ := unstructured.NestedString(binding.Object, "spec", "policyName")
	if policyName != bootstrapPolicyName {
		t.Fatalf("policyName = %q", policyName)
	}

	actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if len(actions) != 1 || actions[0] != "Deny" {
		t.Fatalf("validationActions = %v, want [Deny]", actions)
	}

	policy := supportObject(t, "ValidatingAdmissionPolicy", bootstrapPolicyName)

	failurePolicy, _, _ := unstructured.NestedString(policy.Object, "spec", "failurePolicy")
	if failurePolicy != "Fail" {
		t.Fatalf("failurePolicy = %q, want Fail", failurePolicy)
	}

	rules, _, _ := unstructured.NestedSlice(policy.Object, "spec", "matchConstraints", "resourceRules")
	if len(rules) != 1 {
		t.Fatalf("resourceRules = %v", rules)
	}

	rule := rules[0].(map[string]any) //nolint:errcheck

	resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
	ops, _, _ := unstructured.NestedStringSlice(rule, "operations")

	if !equalSet(resources, []string{"machines", "machines/status"}) || !equalSet(ops, []string{"CREATE", "UPDATE", "DELETE"}) {
		t.Fatalf("resources = %v operations = %v", resources, ops)
	}
}

func TestBootstrapPolicyMatchCondition(t *testing.T) {
	policy := supportObject(t, "ValidatingAdmissionPolicy", bootstrapPolicyName)

	conditions, _, _ := unstructured.NestedSlice(policy.Object, "spec", "matchConditions")
	if len(conditions) != 1 {
		t.Fatalf("matchConditions = %v", conditions)
	}

	expr := conditions[0].(map[string]any)["expression"].(string) //nolint:errcheck

	for user, want := range map[string]bool{
		bootstrapSAUsername: true,
		"system:serviceaccount:unbounded-system:metalman-controller": false,
		"system:serviceaccount:other:metalman-bootstrap":             false,
		"system:node:node-a": false,
	} {
		got := evalPolicyCEL(t, expr, map[string]any{"request": map[string]any{"userInfo": map[string]any{"username": user}}})
		if got != want {
			t.Fatalf("match(%q) = %v, want %v", user, got, want)
		}
	}
}

func TestBootstrapPolicyValidations(t *testing.T) {
	policy := supportObject(t, "ValidatingAdmissionPolicy", bootstrapPolicyName)

	tpm := map[string]any{"ekPublicKey": "pem-a"}
	boot := map[string]any{"credentialID": "JTI=a", "issuedAt": "2026-01-01T00:00:00Z"}

	machine := func(status map[string]any) map[string]any {
		obj := map[string]any{"metadata": map[string]any{"name": "node-a"}}
		if status != nil {
			obj["status"] = status
		}

		return obj
	}

	attested := machine(map[string]any{"tpm": tpm, "bootstrap": boot})
	withCondition := machine(map[string]any{"tpm": tpm, "bootstrap": boot, "conditions": []any{map[string]any{"type": "AgentBootstrapped"}}})
	extra := func(ids ...string) map[string]any { return map[string]any{credentialIDExtra: toAny(ids)} }

	tests := []struct {
		name        string
		operation   string
		subResource string
		extra       map[string]any
		oldObject   any
		object      any
		want        bool
	}{
		{name: "own status update", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: attested, object: withCondition, want: true},
		{name: "other credential", operation: "UPDATE", subResource: "status", extra: extra("JTI=b"), oldObject: attested, object: withCondition},
		{name: "no extra", operation: "UPDATE", subResource: "status", extra: map[string]any{}, oldObject: attested, object: withCondition},
		{name: "empty credential list", operation: "UPDATE", subResource: "status", extra: extra(), oldObject: attested, object: withCondition},
		{name: "machine without status", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: machine(nil), object: machine(map[string]any{"conditions": []any{}})},
		{name: "machine without bootstrap", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: machine(map[string]any{"tpm": tpm}), object: machine(map[string]any{"tpm": tpm})},
		{
			name: "machine with empty credential", operation: "UPDATE", subResource: "status", extra: extra(""),
			oldObject: machine(map[string]any{"bootstrap": map[string]any{"credentialID": ""}}),
			object:    machine(map[string]any{"bootstrap": map[string]any{"credentialID": ""}}),
		},
		{name: "changes tpm", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: attested, object: machine(map[string]any{"tpm": map[string]any{"ekPublicKey": "pem-b"}, "bootstrap": boot})},
		{name: "removes tpm", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: attested, object: machine(map[string]any{"bootstrap": boot})},
		{
			name: "adds tpm", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"),
			oldObject: machine(map[string]any{"bootstrap": boot}), object: machine(map[string]any{"bootstrap": boot, "tpm": tpm}),
		},
		{
			name: "no tpm unchanged", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"),
			oldObject: machine(map[string]any{"bootstrap": boot}), object: machine(map[string]any{"bootstrap": boot}), want: true,
		},
		{
			name: "changes bootstrap", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"),
			oldObject: attested, object: machine(map[string]any{"tpm": tpm, "bootstrap": map[string]any{"credentialID": "JTI=b"}}),
		},
		{name: "removes bootstrap", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: attested, object: machine(map[string]any{"tpm": tpm})},
		{name: "drops whole status", operation: "UPDATE", subResource: "status", extra: extra("JTI=a"), oldObject: attested, object: machine(nil)},
		{name: "create machine", operation: "CREATE", extra: extra("JTI=a"), oldObject: nil, object: attested},
		{name: "update machine", operation: "UPDATE", extra: extra("JTI=a"), oldObject: attested, object: attested},
		{name: "delete machine", operation: "DELETE", extra: extra("JTI=a"), oldObject: attested, object: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activation := map[string]any{
				"request": map[string]any{
					"operation":   tt.operation,
					"subResource": tt.subResource,
					"userInfo":    map[string]any{"username": bootstrapSAUsername, "extra": tt.extra},
				},
				"oldObject": tt.oldObject,
				"object":    tt.object,
			}

			if got := policyAllows(t, policy, activation); got != tt.want {
				t.Fatalf("allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func policyAllows(t *testing.T, policy *unstructured.Unstructured, activation map[string]any) bool {
	t.Helper()

	validations, found, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || !found || len(validations) == 0 {
		t.Fatalf("read spec.validations: found=%v err=%v", found, err)
	}

	for _, v := range validations {
		fields := v.(map[string]any) //nolint:errcheck
		if fields["reason"] != "Forbidden" {
			t.Fatalf("validation %q reason = %v, want Forbidden", fields["message"], fields["reason"])
		}

		if !evalPolicyCEL(t, fields["expression"].(string), activation) { //nolint:errcheck
			return false
		}
	}

	return true
}

// evalPolicyCEL compiles and evaluates expression the way the API server does
// for a dynamically typed object. An evaluation error fails the test: with
// failurePolicy Fail it would deny, and the policy is written so expected
// requests never error.
func evalPolicyCEL(t *testing.T, expression string, activation map[string]any) bool {
	t.Helper()

	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("oldObject", cel.DynType),
		cel.Variable("request", cel.DynType),
	)
	if err != nil {
		t.Fatalf("create CEL environment: %v", err)
	}

	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatalf("compile CEL expression %q: %v", expression, issues.Err())
	}

	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("create CEL program: %v", err)
	}

	for _, key := range []string{"object", "oldObject"} {
		if _, ok := activation[key]; !ok {
			activation[key] = nil
		}
	}

	result, _, err := program.Eval(activation)
	if err != nil {
		t.Fatalf("evaluate CEL expression %q: %v", expression, err)
	}

	allowed, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("CEL result = %T(%v), want bool", result.Value(), result.Value())
	}

	return allowed
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}

	return out
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}

	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}

	return true
}
