// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"testing"

	"github.com/google/cel-go/cel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	wireGuardPortAnnotation          = "net.unbounded-cloud.io/wireguard-port"
	nodeServiceAccountUsername       = "system:serviceaccount:unbounded-system:unbounded-net-node"
	controllerServiceAccountUsername = "system:serviceaccount:unbounded-system:unbounded-net-controller"
)

func TestNodeFieldPolicyProtectsWireGuardPort(t *testing.T) {
	t.Parallel()

	policy := plannedNetObject(t, "ValidatingAdmissionPolicy", "unbounded-net-node-field-restriction")
	expression := policyValidationExpression(t, policy, "unbounded-net-node may not modify the controller-owned WireGuard port annotation")

	tests := []struct {
		name           string
		username       string
		oldAnnotations map[string]string
		annotations    map[string]string
		wantAllowed    bool
	}{
		{name: "node absent", username: nodeServiceAccountUsername, wantAllowed: true},
		{
			name:           "node unchanged",
			username:       nodeServiceAccountUsername,
			oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"},
			annotations:    map[string]string{wireGuardPortAnnotation: "51821"},
			wantAllowed:    true,
		},
		{
			name:           "node changes another annotation",
			username:       nodeServiceAccountUsername,
			oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"},
			annotations: map[string]string{
				wireGuardPortAnnotation:             "51821",
				"net.unbounded-cloud.io/tunnel-mtu": "1400",
			},
			wantAllowed: true,
		},
		{name: "node adds", username: nodeServiceAccountUsername, annotations: map[string]string{wireGuardPortAnnotation: "51821"}},
		{
			name:           "node changes",
			username:       nodeServiceAccountUsername,
			oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"},
			annotations:    map[string]string{wireGuardPortAnnotation: "51822"},
		},
		{name: "node removes", username: nodeServiceAccountUsername, oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"}},
		{name: "controller adds", username: controllerServiceAccountUsername, annotations: map[string]string{wireGuardPortAnnotation: "51821"}, wantAllowed: true},
		{
			name:           "controller changes",
			username:       controllerServiceAccountUsername,
			oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"},
			annotations:    map[string]string{wireGuardPortAnnotation: "51822"},
			wantAllowed:    true,
		},
		{name: "controller removes", username: controllerServiceAccountUsername, oldAnnotations: map[string]string{wireGuardPortAnnotation: "51821"}, wantAllowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := evalCEL(t, expression, map[string]any{
				"oldObject": nodeAdmissionObject(tt.oldAnnotations),
				"object":    nodeAdmissionObject(tt.annotations),
				"request": map[string]any{
					"userInfo": map[string]any{"username": tt.username},
				},
			})
			if got != tt.wantAllowed {
				t.Fatalf("allowed = %v, want %v", got, tt.wantAllowed)
			}
		})
	}
}

func plannedNetObject(t *testing.T, kind, name string) *unstructured.Unstructured {
	t.Helper()

	plan, _, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, op := range plan.Operations {
		if op.Object.GetKind() == kind && op.Object.GetName() == name {
			return op.Object
		}
	}

	t.Fatalf("plan does not contain %s/%s", kind, name)

	return nil
}

func policyValidationExpression(t *testing.T, policy *unstructured.Unstructured, message string) string {
	t.Helper()

	validations, found, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || !found {
		t.Fatalf("read spec.validations: found=%v err=%v", found, err)
	}

	for _, validation := range validations {
		fields := validation.(map[string]any) //nolint:errcheck
		if fields["message"] == message {
			return fields["expression"].(string) //nolint:errcheck
		}
	}

	t.Fatalf("policy has no validation with message %q", message)

	return ""
}

func nodeAdmissionObject(annotations map[string]string) map[string]any {
	metadata := map[string]any{}
	if annotations != nil {
		metadata["annotations"] = annotations
	}

	return map[string]any{"metadata": metadata}
}

func evalCEL(t *testing.T, expression string, activation map[string]any) bool {
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
