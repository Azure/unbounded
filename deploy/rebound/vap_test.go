// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rebound

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
)

func evalCEL(t *testing.T, expression string, activation map[string]any) ref.Val {
	t.Helper()

	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType),
		cel.Variable("request", cel.DynType), cel.Variable("params", cel.DynType),
		cel.Variable("variables", cel.DynType), cel.OptionalTypes(), ext.Strings(),
	)
	require.NoError(t, err)

	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err(), expression)

	program, err := env.Program(ast)
	require.NoError(t, err)
	value, _, err := program.Eval(activation)
	require.NoError(t, err, expression)

	return value
}

func policyAllows(t *testing.T, policy admissionv1.ValidatingAdmissionPolicy, activation map[string]any) bool {
	t.Helper()

	for _, condition := range policy.Spec.MatchConditions {
		if !evalCEL(t, condition.Expression, activation).Value().(bool) {
			return true
		}
	}

	variables := map[string]any{}
	activation["variables"] = variables

	for _, variable := range policy.Spec.Variables {
		variables[variable.Name] = evalCEL(t, variable.Expression, activation)
	}

	allowed := true

	for _, validation := range policy.Spec.Validations {
		if !evalCEL(t, validation.Expression, activation).Value().(bool) {
			allowed = false

			if validation.MessageExpression != "" {
				message := evalCEL(t, validation.MessageExpression, activation).Value().(string)
				require.Equal(t, "node node-a is reserved by grant team-a/grant-a", message)
			}
		}
	}

	return allowed
}

// podRequest builds an admission request the way the API server does: it
// omits subResource when it is empty.
func podRequest(namespace, resource, subresource string) map[string]any {
	request := map[string]any{"namespace": namespace, "resource": map[string]any{"resource": resource}}
	if subresource != "" {
		request["subResource"] = subresource
	}

	return request
}

func TestPodPlacementCEL(t *testing.T) {
	t.Parallel()

	objects := renderObjects(t, nil)
	policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-pod-placement")
	ledger := map[string]any{"data": map[string]string{"node-a": "team-a/grant-a", "_grant.team-a.grant-a": "{}"}}

	for _, tc := range []struct {
		name, namespace, node, resource, subresource string
		user                                         string
		mirror                                       bool
		params                                       any
		allowed                                      bool
	}{
		{"free node", "team-b", "free", "pods", "", "user", false, ledger, true},
		{"same namespace", "team-a", "node-a", "pods", "", "user", false, ledger, true},
		{"other namespace", "team-b", "node-a", "pods", "", "user", false, ledger, false},
		{"namespace prefix", "team", "node-a", "pods", "", "user", false, ledger, false},
		{"excluded system", "kube-system", "node-a", "pods", "", "user", false, ledger, true},
		{"excluded operator", "unbounded-system", "node-a", "pods", "", "user", false, ledger, true},
		{"mirror", "team-b", "node-a", "pods", "", "system:node:node-a", true, ledger, true},
		{"mirror by user denied", "team-b", "node-a", "pods", "", "user", true, ledger, false},
		{"mirror by other node denied", "team-b", "node-a", "pods", "", "system:node:other", true, ledger, false},
		{"node without mirror denied", "team-b", "node-a", "pods", "", "system:node:node-a", false, ledger, false},
		{"unscheduled", "team-b", "", "pods", "", "user", false, ledger, true},
		{"binding denied", "team-b", "node-a", "pods", "binding", "user", false, ledger, false},
		{"binding same namespace", "team-a", "node-a", "pods", "binding", "user", false, ledger, true},
		{"bindings denied", "team-b", "node-a", "bindings", "", "user", false, ledger, false},
		{"bindings same namespace", "team-a", "node-a", "bindings", "", "user", false, ledger, true},
		{"binding mirror annotation does not bypass", "team-b", "node-a", "pods", "binding", "system:node:node-a", true, ledger, false},
		{"missing params", "team-b", "node-a", "pods", "", "user", false, nil, true},
		{"missing data", "team-b", "node-a", "pods", "", "user", false, map[string]any{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]any{}
			if tc.mirror {
				metadata["annotations"] = map[string]string{"kubernetes.io/config.mirror": "hash"}
			}

			obj := map[string]any{"metadata": metadata}
			if tc.resource == "pods" && tc.subresource == "" {
				obj["spec"] = map[string]any{}
				if tc.node != "" {
					obj["spec"] = map[string]any{"nodeName": tc.node}
				}
			} else {
				obj["target"] = map[string]any{"kind": "Node", "name": tc.node}
			}

			request := podRequest(tc.namespace, tc.resource, tc.subresource)
			request["userInfo"] = map[string]any{"username": tc.user}
			require.Equal(t, tc.allowed, policyAllows(t, policy, map[string]any{"object": obj, "params": tc.params, "request": request}))
		})
	}
}

func TestBindingTargetKindCEL(t *testing.T) {
	t.Parallel()

	objects := renderObjects(t, nil)
	policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-pod-placement")
	ledger := map[string]any{"data": map[string]string{"node-a": "team-a/grant-a"}}

	for _, resource := range []string{"pods", "bindings"} {
		for _, target := range []struct {
			name   string
			fields map[string]any
		}{
			{"missing kind", map[string]any{"name": "node-a"}},
			{"empty kind", map[string]any{"kind": "", "name": "node-a"}},
			{"Node kind", map[string]any{"kind": "Node", "name": "node-a"}},
		} {
			for _, namespace := range []string{"team-a", "team-b"} {
				t.Run(resource+"/"+target.name+"/"+namespace, func(t *testing.T) {
					subresource := ""
					if resource == "pods" {
						subresource = "binding"
					}

					require.Equal(t, namespace == "team-a", policyAllows(t, policy, map[string]any{
						"object":  map[string]any{"metadata": map[string]any{}, "target": target.fields},
						"params":  ledger,
						"request": podRequest(namespace, resource, subresource),
					}))
				})
			}
		}
	}
}

func nodeObject(labels map[string]string, taints ...map[string]any) map[string]any {
	metadata := map[string]any{}
	if labels != nil {
		metadata["labels"] = labels
	}

	spec := map[string]any{}
	if taints != nil {
		spec["taints"] = taints
	}

	return map[string]any{"metadata": metadata, "spec": spec}
}

func TestNodeKeysCEL(t *testing.T) {
	t.Parallel()

	objects := renderObjects(t, nil)
	policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-node-keys")
	plain := nodeObject(nil)
	labeled := nodeObject(map[string]string{"rebound.unbounded-cloud.io/namespace": "team-a"})
	grantLabel := nodeObject(map[string]string{"rebound.unbounded-cloud.io/grant": "grant-a"})
	tainted := nodeObject(nil, map[string]any{"key": "rebound.unbounded-cloud.io/granted", "value": "team-a", "effect": "PreferNoSchedule"})
	changedTaint := nodeObject(nil, map[string]any{"key": "rebound.unbounded-cloud.io/granted", "value": "team-b", "effect": "PreferNoSchedule"})

	for _, tc := range []struct {
		name, user string
		groups     []string
		old, obj   any
		allowed    bool
	}{
		{"rebound SA", "system:serviceaccount:unbounded-system:rebound", nil, plain, labeled, true},
		{"admin", "admin", []string{"system:masters"}, plain, tainted, true},
		{"label added", "user", nil, plain, labeled, false},
		{"grant label added", "user", nil, plain, grantLabel, false},
		{"label removed", "user", nil, labeled, plain, false},
		{"label unchanged", "user", nil, labeled, labeled, true},
		{"empty protected label", "user", nil, plain, nodeObject(map[string]string{"rebound.unbounded-cloud.io/grant": ""}), false},
		{"taint added", "user", nil, plain, tainted, false},
		{"taint removed", "user", nil, tainted, plain, false},
		{"taint changed", "user", nil, tainted, changedTaint, false},
		{"taint unchanged", "user", nil, tainted, tainted, true},
		{"unrelated label", "user", nil, plain, nodeObject(map[string]string{"example.test/key": "value"}), true},
		{"unrelated taint", "user", nil, plain, nodeObject(nil, map[string]any{"key": "other", "effect": "NoSchedule"}), true},
		{"create protected label", "user", nil, nil, labeled, false},
		{"create protected taint", "user", nil, nil, tainted, false},
		{"create plain", "user", nil, nil, plain, true},
		{"wrong namespace SA", "system:serviceaccount:other:rebound", nil, plain, labeled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userInfo := map[string]any{"username": tc.user}
			if tc.groups != nil {
				userInfo["groups"] = tc.groups
			}

			require.Equal(t, tc.allowed, policyAllows(t, policy, map[string]any{
				"object": tc.obj, "oldObject": tc.old, "request": map[string]any{"userInfo": userInfo},
			}))
		})
	}
}

func TestNodeStatusKeysCEL(t *testing.T) {
	t.Parallel()

	objects := renderObjects(t, nil)
	policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-node-keys")

	for _, key := range []string{"rebound.unbounded-cloud.io/namespace", "rebound.unbounded-cloud.io/grant"} {
		for _, tc := range []struct {
			name        string
			old, labels map[string]string
			allowed     bool
		}{
			{"added", nil, map[string]string{key: "team-a"}, false},
			{"removed", map[string]string{key: "team-a"}, nil, false},
			{"changed", map[string]string{key: "team-a"}, map[string]string{key: "team-b"}, false},
			{"unchanged", map[string]string{key: "team-a"}, map[string]string{key: "team-a"}, true},
			{"unrelated", nil, map[string]string{"example.test/key": "value"}, true},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				obj := nodeObject(tc.labels)
				obj["status"] = map[string]any{"phase": "Running"}

				require.Equal(t, tc.allowed, policyAllows(t, policy, map[string]any{
					"object": obj, "oldObject": nodeObject(tc.old),
					"request": map[string]any{
						"resource": map[string]any{"resource": "nodes"}, "subResource": "status",
						"userInfo": map[string]any{"username": "system:node:node-a", "groups": []string{"system:nodes"}},
					},
				}))
			})
		}
	}
}

func TestPolicyOverridesCEL(t *testing.T) {
	t.Parallel()

	objects := renderObjects(t, map[string]string{"Namespace": "grants", "AdminGroups": "operators,admins", "ExcludedNamespaces": "platform,grants"})
	placement := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-pod-placement")

	for _, namespace := range []string{"platform", "grants", "kube-system"} {
		require.Equal(t, namespace != "kube-system", policyAllows(t, placement, map[string]any{
			"object":  map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeName": "node-a"}},
			"params":  map[string]any{"data": map[string]string{"node-a": "team-a/grant-a"}},
			"request": podRequest(namespace, "pods", ""),
		}))
	}

	keys := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-node-keys")
	for _, group := range []string{"operators", "admins", "system:masters"} {
		require.Equal(t, group != "system:masters", policyAllows(t, keys, map[string]any{
			"oldObject": nil, "object": nodeObject(map[string]string{"rebound.unbounded-cloud.io/grant": "grant-a"}),
			"request": map[string]any{"userInfo": map[string]any{"username": "user", "groups": []string{group}}},
		}))
	}

	require.True(t, policyAllows(t, keys, map[string]any{
		"oldObject": nil, "object": nodeObject(map[string]string{"rebound.unbounded-cloud.io/grant": "grant-a"}),
		"request": map[string]any{"userInfo": map[string]any{"username": "system:serviceaccount:grants:rebound"}},
	}))
}
