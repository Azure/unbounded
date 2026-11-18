// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package centerpivot

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/centerpivot/contract"
	"github.com/Azure/unbounded/internal/centerpivot/controller"
)

const (
	serviceUser  = "system:serviceaccount:unbounded-system:centerpivot"
	nodeInfoUser = "system:serviceaccount:unbounded-system:centerpivot-node-info"
	nvidiaAppID  = "22222222-2222-3333-4444-555555555555"
)

func plannedPolicy(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()

	other := site("core", true)
	other.Spec.Components.CenterPivot.NebraskaURL = "https://other.example.com/v1/update"
	other.Spec.Components.CenterPivot.Apps = []unboundedv1alpha3.CenterPivotApp{{Name: "acl", AppID: "99999999-2222-3333-4444-555555555555"}}

	edge := site("edge", true)
	edge.Spec.Components.CenterPivot.Apps = append(edge.Spec.Components.CenterPivot.Apps,
		unboundedv1alpha3.CenterPivotApp{Name: "acl-nvidia", AppID: nvidiaAppID})

	plan, _, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{edge, other})
	require.NoError(t, err)

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "ValidatingAdmissionPolicy" && op.Object.GetName() == name {
			return op.Object
		}
	}

	t.Fatalf("plan has no policy %s", name)

	return nil
}

func evaluate(t *testing.T, expression string, activation map[string]any) ref.Val {
	t.Helper()

	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("oldObject", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.DynType),
		cel.OptionalTypes(),
	)
	require.NoError(t, err)

	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err(), expression)

	program, err := env.Program(ast)
	require.NoError(t, err)

	result, _, err := program.Eval(activation)
	require.NoError(t, err, expression)

	return result
}

// admits reports whether the policy lets user change oldObject to object.
func admits(t *testing.T, policy *unstructured.Unstructured, user, nodeName string, oldObject, object map[string]any) bool {
	t.Helper()

	activation := map[string]any{
		"object":    object,
		"oldObject": oldObject,
		"request": map[string]any{"userInfo": map[string]any{
			"username": user,
			"extra": map[string][]string{
				"authentication.kubernetes.io/node-name": {nodeName},
				"authentication.kubernetes.io/node-uid":  {nodeName + "-uid"},
			},
		}},
	}

	variables := map[string]any{}

	declared, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
	for _, variable := range declared {
		fields := variable.(map[string]any)                                                         //nolint:errcheck
		variables[fields["name"].(string)] = evaluate(t, fields["expression"].(string), activation) //nolint:errcheck
	}

	activation["variables"] = variables

	for _, field := range []string{"matchConditions", "validations"} {
		rules, _, _ := unstructured.NestedSlice(policy.Object, "spec", field)
		for _, rule := range rules {
			passed, ok := evaluate(t, rule.(map[string]any)["expression"].(string), activation).Value().(bool) //nolint:errcheck
			require.True(t, ok)

			if !passed {
				return field == "matchConditions"
			}
		}
	}

	return true
}

func node(annotations map[string]any, taints ...any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": "acl-1", "uid": "acl-1-uid",
			"labels":      map[string]any{"kubernetes.io/arch": "amd64"},
			"annotations": annotations,
		},
		"spec":   map[string]any{"podCIDR": "10.0.0.0/24", "taints": taints},
		"status": map[string]any{"phase": "Running"},
	}
}

func updating(effect string) map[string]any {
	return map[string]any{"key": controller.UpdatingTaint, "effect": effect}
}

func request(t *testing.T, server, appID string) string {
	t.Helper()

	value, err := contract.NewRequest(contract.OperationStage, "", "202606.29.0",
		contract.Source{Server: server, AppID: appID, Track: "stable"}).Marshal()
	require.NoError(t, err)

	return value
}

func TestNodePolicy(t *testing.T) {
	t.Parallel()

	policy := plannedPolicy(t, nodePolicyName)
	gpu := map[string]any{"key": "gpu", "effect": "NoSchedule"}
	info := map[string]any{controller.AgentInfoAnnotation: "{}"}

	withRequest := func(value string) map[string]any {
		return map[string]any{controller.AgentInfoAnnotation: "{}", controller.Keys.Request: value}
	}

	tests := []struct {
		name           string
		user           string
		before, after  map[string]any
		wantAdmitted   bool
		changeMetadata func(map[string]any)
	}{
		{name: "add its taint", before: node(info, gpu), after: node(info, gpu, updating("NoSchedule")), wantAdmitted: true},
		{name: "remove its taint", before: node(info, updating("NoSchedule"), gpu), after: node(info, gpu), wantAdmitted: true},
		{name: "its taint with another effect", before: node(info), after: node(info, updating("NoExecute"))},
		{name: "another taint", before: node(info, gpu), after: node(info)},
		{name: "a request to an enabled Site's Nebraska", before: node(info), after: node(withRequest(request(t, server, appID))), wantAdmitted: true},
		{name: "a request for the Site's other app", before: node(info), after: node(withRequest(request(t, server, nvidiaAppID))), wantAdmitted: true},
		{
			name:   "a request to the other enabled Site's Nebraska",
			before: node(info), after: node(withRequest(request(t, "https://other.example.com/v1/update", "99999999-2222-3333-4444-555555555555"))),
			wantAdmitted: true,
		},
		{name: "a request to another Nebraska", before: node(info), after: node(withRequest(request(t, "https://evil.example.com/v1/update", appID)))},
		{name: "a request mixing two Sites' Nebraska", before: node(info), after: node(withRequest(request(t, server, "99999999-2222-3333-4444-555555555555")))},
		{name: "a request to a lookalike URL", before: node(info), after: node(withRequest(request(t, server+".evil.example.com", appID)))},
		{name: "another annotation", before: node(info), after: node(map[string]any{controller.AgentInfoAnnotation: "forged"})},
		{
			name: "a label", before: node(info), after: node(info),
			changeMetadata: func(metadata map[string]any) { metadata["labels"] = map[string]any{"kubernetes.io/arch": "arm64"} },
		},
		{name: "anyone else", user: "system:node:acl-1", before: node(info), after: node(withRequest("anything")), wantAdmitted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.changeMetadata != nil {
				tt.changeMetadata(tt.after["metadata"].(map[string]any)) //nolint:errcheck
			}

			user := tt.user
			if user == "" {
				user = serviceUser
			}

			assert.Equal(t, tt.wantAdmitted, admits(t, policy, user, "", tt.before, tt.after))
		})
	}
}

func TestMachinePolicy(t *testing.T) {
	t.Parallel()

	policy := plannedPolicy(t, "centerpivot-machine-restriction")

	machine := func(annotations map[string]any, image string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "acl-1", "annotations": annotations},
			"spec":     map[string]any{"image": image},
		}
	}
	before := machine(map[string]any{"owner": "machina"}, "acl")

	assert.True(t, admits(t, policy, serviceUser, "", before, machine(map[string]any{"owner": "machina", controller.StateAnnotation: "{}"}, "acl")))
	assert.False(t, admits(t, policy, serviceUser, "", before, machine(map[string]any{"owner": "centerpivot"}, "acl")))
	assert.False(t, admits(t, policy, serviceUser, "", before, machine(map[string]any{"owner": "machina"}, "ubuntu")))
}

func TestNodeInfoPolicy(t *testing.T) {
	t.Parallel()

	policy := plannedPolicy(t, "centerpivot-node-info-restriction")
	before := node(map[string]any{"other": "x"})

	withInfo := node(map[string]any{"other": "x", controller.AgentInfoAnnotation: "{}"})
	assert.True(t, admits(t, policy, nodeInfoUser, "acl-1", before, withInfo), "its own Node's agent-info")
	assert.False(t, admits(t, policy, nodeInfoUser, "acl-2", before, withInfo), "another Node")
	assert.False(t, admits(t, policy, nodeInfoUser, "acl-1", before, node(map[string]any{"other": "y"})), "another annotation")
	assert.False(t, admits(t, policy, nodeInfoUser, "acl-1", before, node(map[string]any{"other": "x"}, updating("NoSchedule"))), "a taint")
}
