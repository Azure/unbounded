// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"reflect"
	"testing"

	"github.com/google/cel-go/cel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	wireGuardPortAnnotation               = "net.unbounded-cloud.io/wireguard-port"
	discoveredPublicIPAnnotation          = "net.unbounded-cloud.io/discovered-public-ip"
	discoveredPublicIPExpiresAtAnnotation = "net.unbounded-cloud.io/discovered-public-ip-expires-at"
	declaredPublicIPAnnotation            = "net.unbounded-cloud.io/declared-public-ip"
	nodeServiceAccountUsername            = "system:serviceaccount:unbounded-system:unbounded-net-node"
	controllerServiceAccountUsername      = "system:serviceaccount:unbounded-system:unbounded-net-controller"
	nodeNameExtra                         = "authentication.kubernetes.io/node-name"
	nodeUIDExtra                          = "authentication.kubernetes.io/node-uid"
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

func TestPublicIPAnnotationOwnershipPolicyScope(t *testing.T) {
	t.Parallel()

	policy := plannedNetObject(t, "ValidatingAdmissionPolicy", "unbounded-net-public-ip-annotation-ownership")

	failurePolicy, found, err := unstructured.NestedString(policy.Object, "spec", "failurePolicy")
	if err != nil || !found || failurePolicy != "Fail" {
		t.Fatalf("spec.failurePolicy = %q, found=%v err=%v, want Fail", failurePolicy, found, err)
	}

	if _, matchConditionsFound, matchConditionsErr := unstructured.NestedFieldNoCopy(
		policy.Object,
		"spec",
		"matchConditions",
	); matchConditionsErr != nil || matchConditionsFound {
		t.Fatalf("spec.matchConditions: found=%v err=%v, want absent", matchConditionsFound, matchConditionsErr)
	}

	rules, found, err := unstructured.NestedSlice(policy.Object, "spec", "matchConstraints", "resourceRules")
	if err != nil || !found || len(rules) != 1 {
		t.Fatalf("spec.matchConstraints.resourceRules: found=%v len=%d err=%v, want one rule", found, len(rules), err)
	}

	rule := rules[0].(map[string]any) //nolint:errcheck
	assertStringSliceField(t, rule, []string{""}, "apiGroups")
	assertStringSliceField(t, rule, []string{"v1"}, "apiVersions")
	assertStringSliceField(t, rule, []string{"CREATE", "UPDATE"}, "operations")
	assertStringSliceField(t, rule, []string{"nodes"}, "resources")

	validations, found, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || !found || len(validations) != 2 {
		t.Fatalf("spec.validations: found=%v len=%d err=%v, want two validations", found, len(validations), err)
	}

	binding := plannedNetObject(t, "ValidatingAdmissionPolicyBinding", "unbounded-net-public-ip-annotation-ownership")

	policyName, found, err := unstructured.NestedString(binding.Object, "spec", "policyName")
	if err != nil || !found || policyName != policy.GetName() {
		t.Fatalf("binding spec.policyName = %q, found=%v err=%v, want %q", policyName, found, err, policy.GetName())
	}

	actions, found, err := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if err != nil || !found || !reflect.DeepEqual(actions, []string{"Deny"}) {
		t.Fatalf("binding spec.validationActions = %v, found=%v err=%v, want [Deny]", actions, found, err)
	}
}

func TestPublicIPAnnotationOwnershipPolicy(t *testing.T) {
	t.Parallel()

	policy := plannedNetObject(t, "ValidatingAdmissionPolicy", "unbounded-net-public-ip-annotation-ownership")
	nodeTokenExtras := func(name, uid string) map[string][]string {
		return map[string][]string{
			nodeNameExtra: {name},
			nodeUIDExtra:  {uid},
		}
	}

	tests := []struct {
		name           string
		username       string
		extra          map[string][]string
		oldAnnotations map[string]string
		annotations    map[string]string
		create         bool
		wantAllowed    bool
	}{
		{
			name:     "node agent adds discovery for its node",
			username: nodeServiceAccountUsername,
			extra:    nodeTokenExtras("node-a", "uid-a"),
			annotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.10",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z",
				declaredPublicIPAnnotation:            "192.0.2.20",
			},
			oldAnnotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			wantAllowed:    true,
		},
		{
			name:     "node agent changes discovery for its node",
			username: nodeServiceAccountUsername,
			extra:    nodeTokenExtras("node-a", "uid-a"),
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.10",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z",
			},
			annotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.11",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T13:00:00Z",
			},
			wantAllowed: true,
		},
		{
			name:        "cross-node agent adds discovery",
			username:    nodeServiceAccountUsername,
			extra:       nodeTokenExtras("node-b", "uid-a"),
			annotations: map[string]string{discoveredPublicIPAnnotation: "192.0.2.10"},
		},
		{
			name:     "cross-node agent changes discovery",
			username: nodeServiceAccountUsername,
			extra:    nodeTokenExtras("node-b", "uid-a"),
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.10",
			},
			annotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.11",
			},
		},
		{
			name:        "node UID mismatch",
			username:    nodeServiceAccountUsername,
			extra:       nodeTokenExtras("node-a", "uid-b"),
			annotations: map[string]string{discoveredPublicIPAnnotation: "192.0.2.10"},
		},
		{
			name:        "node token extras missing",
			username:    nodeServiceAccountUsername,
			extra:       map[string][]string{},
			annotations: map[string]string{discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z"},
		},
		{
			name:        "unrelated service account adds discovery with matching extras",
			username:    "system:serviceaccount:unbounded-system:unbounded-storage-supervisor",
			extra:       nodeTokenExtras("node-a", "uid-a"),
			annotations: map[string]string{discoveredPublicIPAnnotation: "192.0.2.10"},
		},
		{
			name:     "unrelated service account changes discovery with matching extras",
			username: "system:serviceaccount:unbounded-system:unbounded-storage-supervisor",
			extra:    nodeTokenExtras("node-a", "uid-a"),
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.10",
			},
			annotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.11",
			},
		},
		{
			name:     "human changes discovery",
			username: "operator@example.com",
			extra:    nodeTokenExtras("node-a", "uid-a"),
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.10",
			},
			annotations: map[string]string{
				discoveredPublicIPAnnotation: "192.0.2.11",
			},
		},
		{
			name:     "unchanged discovery is accepted from any caller",
			username: "system:node:node-a",
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.10",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z",
			},
			annotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.10",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z",
			},
			wantAllowed: true,
		},
		{
			name:     "discovery deletion is accepted from any caller",
			username: "system:serviceaccount:unbounded-system:unbounded-storage-supervisor",
			oldAnnotations: map[string]string{
				discoveredPublicIPAnnotation:          "192.0.2.10",
				discoveredPublicIPExpiresAtAnnotation: "2026-08-26T12:00:00Z",
			},
			wantAllowed: true,
		},
		{
			name:        "discovery on create is rejected from a human",
			username:    "operator@example.com",
			annotations: map[string]string{discoveredPublicIPAnnotation: "192.0.2.10"},
			create:      true,
		},
		{
			name:        "human adds declared address",
			username:    "operator@example.com",
			annotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			wantAllowed: true,
		},
		{
			name:           "human changes declared address",
			username:       "operator@example.com",
			oldAnnotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			annotations:    map[string]string{declaredPublicIPAnnotation: "192.0.2.21"},
			wantAllowed:    true,
		},
		{
			name:        "human adds declared address on create",
			username:    "operator@example.com",
			annotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			create:      true,
			wantAllowed: true,
		},
		{
			name:        "service account adds declared address",
			username:    "system:serviceaccount:unbounded-system:unbounded-storage-supervisor",
			annotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
		},
		{
			name:           "node agent changes declared address",
			username:       nodeServiceAccountUsername,
			extra:          nodeTokenExtras("node-a", "uid-a"),
			oldAnnotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			annotations:    map[string]string{declaredPublicIPAnnotation: "192.0.2.21"},
		},
		{
			name:        "kubelet adds declared address",
			username:    "system:node:node-a",
			annotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
		},
		{
			name:           "unchanged declared address is accepted from any caller",
			username:       nodeServiceAccountUsername,
			oldAnnotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			annotations:    map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			wantAllowed:    true,
		},
		{
			name:           "declared address deletion is accepted from any caller",
			username:       "system:node:node-a",
			oldAnnotations: map[string]string{declaredPublicIPAnnotation: "192.0.2.20"},
			wantAllowed:    true,
		},
		{
			name:        "present empty annotations are accepted",
			username:    nodeServiceAccountUsername,
			annotations: map[string]string{},
			wantAllowed: true,
		},
		{
			name:        "absent annotations are accepted",
			username:    nodeServiceAccountUsername,
			wantAllowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var oldObject any = publicIPNodeAdmissionObject("node-a", "uid-a", tt.oldAnnotations)
			if tt.create {
				oldObject = nil
			}

			extra := tt.extra
			if extra == nil {
				extra = map[string][]string{}
			}

			got := policyAllows(t, policy, map[string]any{
				"oldObject": oldObject,
				"object":    publicIPNodeAdmissionObject("node-a", "uid-a", tt.annotations),
				"request": map[string]any{
					"userInfo": map[string]any{
						"username": tt.username,
						"extra":    extra,
					},
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

func policyAllows(t *testing.T, policy *unstructured.Unstructured, activation map[string]any) bool {
	t.Helper()

	validations, found, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || !found {
		t.Fatalf("read spec.validations: found=%v err=%v", found, err)
	}

	for _, validation := range validations {
		fields := validation.(map[string]any) //nolint:errcheck

		expression := fields["expression"].(string) //nolint:errcheck
		if !evalCEL(t, expression, activation) {
			return false
		}
	}

	return true
}

func assertStringSliceField(t *testing.T, object map[string]any, want []string, fields ...string) {
	t.Helper()

	got, found, err := unstructured.NestedStringSlice(object, fields...)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v = %v, found=%v err=%v, want %v", fields, got, found, err, want)
	}
}

func nodeAdmissionObject(annotations map[string]string) map[string]any {
	metadata := map[string]any{}
	if annotations != nil {
		metadata["annotations"] = annotations
	}

	return map[string]any{"metadata": metadata}
}

func publicIPNodeAdmissionObject(name, uid string, annotations map[string]string) map[string]any {
	metadata := map[string]any{
		"name": name,
		"uid":  uid,
	}
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
		cel.OptionalTypes(),
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
