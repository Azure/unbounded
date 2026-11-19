// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

const (
	agentDaemonRole        = "unbounded-daemon-controller-machine"
	machineOpsRole         = "azure-machine-ops-controller"
	metalmanControllerRole = "metalman-controller"
)

func TestMachineOperationProviderStatePolicyShape(t *testing.T) {
	t.Parallel()

	failurePolicy := admissionregistrationv1.Fail
	policy, binding := policyDocuments(t, providerStatePolicyFile)

	projection := func(object string) string {
		return object + ".?status.?targets.orValue([]).filter(t, has(t.providerOperation) || has(t.input))" +
			".map(t, {'machineRef': t.machineRef, ?'providerOperation': t.?providerOperation, ?'input': t.?input})"
	}

	require.Equal(t, admissionregistrationv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: &failurePolicy,
		MatchConstraints: &admissionregistrationv1.MatchResources{
			ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"unbounded-cloud.io"},
						APIVersions: []string{"*"},
						Resources:   []string{"machineoperations/status"},
					},
				},
			}},
		},
		Variables: []admissionregistrationv1.Variable{
			{Name: "oldProviderState", Expression: projection("oldObject")},
			{Name: "newProviderState", Expression: projection("object")},
		},
		Validations: []admissionregistrationv1.Validation{{
			Expression: "variables.oldProviderState == variables.newProviderState || " +
				"authorizer.group('unbounded-cloud.io').resource('machineoperations').subresource('finalizers').check('update').allowed()",
			Message: providerStateDenial,
			Reason:  ptrTo(metav1.StatusReasonForbidden),
		}},
	}, policy.Spec)
	require.Equal(t, providerStatePolicyName, policy.Name)
	requireDenyBinding(t, binding, providerStatePolicyName)
}

// The policy tells an operations controller from an agent daemon by update on
// machineoperations/finalizers, so the shipped roles must keep that split.
func TestMachineOperationFinalizersGrantIdentifiesOperationsControllers(t *testing.T) {
	t.Parallel()

	machineOps := renderedClusterRoles(t, ".")
	machina := renderedClusterRoles(t, filepath.Join("..", "machina"))

	require.True(t, allowsFinalizersUpdate(machineOps[machineOpsRole]), machineOpsRole)
	require.True(t, allowsFinalizersUpdate(machina[metalmanControllerRole]), metalmanControllerRole)

	agent, ok := machina[agentDaemonRole]
	require.True(t, ok, "%s not rendered", agentDaemonRole)
	require.False(t, allowsFinalizersUpdate(agent), agentDaemonRole)
	require.True(t, grants(agent.Rules, "unbounded-cloud.io", "machineoperations/status", "update"),
		"the agent daemons no longer update machineoperations/status: revisit the policy")

	// The policy renders whether or not capacity is enabled.
	rendered := renderMachineOps(t, map[string]string{})
	require.Len(t, rendered[strings.TrimSuffix(providerStatePolicyFile, ".tmpl")], 2)
}

func TestEnvtestMachineOperationProviderStatePolicy(t *testing.T) {
	env, admin := startPolicyEnvironment(t)
	ctx := context.Background()

	agent := addPolicyUser(t, env, "agent")
	controller := addPolicyUser(t, env, "machine-ops")
	metalman := addPolicyUser(t, env, "metalman")
	clusterAdmin := addPolicyUser(t, env, "admin-user")

	// Each identity holds exactly the ClusterRole its component ships with.
	machineOps := renderedClusterRoles(t, ".")
	machina := renderedClusterRoles(t, filepath.Join("..", "machina"))

	createAll(
		t, admin,
		machineOps[machineOpsRole], machina[agentDaemonRole], machina[metalmanControllerRole],
		clusterRoleBinding("test-agent", agentDaemonRole, userSubject("agent")),
		clusterRoleBinding("test-machine-ops", machineOpsRole, userSubject("machine-ops")),
		clusterRoleBinding("test-metalman", metalmanControllerRole, userSubject("metalman")),
		clusterRoleBinding("test-admin-user", "cluster-admin", userSubject("admin-user")),
	)

	providerOperation := func(token string) *unboundedv1alpha3.ProviderOperationStatus {
		return &unboundedv1alpha3.ProviderOperationStatus{Provider: "azure", OperationID: "op-1", ResumeToken: token}
	}
	input := func(image string) *unboundedv1alpha3.MachineOperationTargetInput {
		return &unboundedv1alpha3.MachineOperationTargetInput{HostImage: image}
	}

	// update reads the operation and writes its status as c after mutate.
	update := func(c client.Client, name string, mutate func(*unboundedv1alpha3.MachineOperationStatus), opts ...client.SubResourceUpdateOption) error {
		var operation unboundedv1alpha3.MachineOperation
		require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: name}, &operation))
		mutate(&operation.Status)

		return c.Status().Update(ctx, &operation, opts...)
	}

	// seed creates an operation whose status holds two targets with
	// controller-owned state, in-flight and resolved, and one without.
	seed := func(name string) {
		createAll(t, admin, &unboundedv1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: unboundedv1alpha3.MachineOperationSpec{
				MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}},
				OperationKind:   unboundedv1alpha3.OperationHostReplace,
			},
		})
		require.NoError(t, update(controller, name, func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Phase = unboundedv1alpha3.OperationPhaseInProgress
			status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{
				{MachineRef: "owned-a", Phase: unboundedv1alpha3.OperationPhaseInProgress, Input: input("image-a"), ProviderOperation: providerOperation("token-a")},
				{MachineRef: "owned-b", Phase: unboundedv1alpha3.OperationPhasePending, Input: input("image-b")},
				{MachineRef: "plain", Phase: unboundedv1alpha3.OperationPhasePending},
			}
		}))
	}

	seed("seeded")

	// Policies load asynchronously: wait until a request that must be
	// denied is denied.
	waitForDenial(t, func() error {
		return update(agent, "seeded", func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation.ResumeToken = "forged"
		}, client.DryRunAll)
	}, providerStateDenial)

	denied := map[string]func(*unboundedv1alpha3.MachineOperationStatus){
		"change the resume token": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation.ResumeToken = "forged"
		},
		"change the operation ID": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation.OperationID = "op-2"
		},
		"change the provider": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation.Provider = "other"
		},
		"remove providerOperation": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation = nil
		},
		"add providerOperation to a target with input": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[1].ProviderOperation = providerOperation("forged")
		},
		"add providerOperation to a target without either field": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[2].ProviderOperation = providerOperation("forged")
		},
		"change input": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].Input.HostImage = "forged"
		},
		"extend input": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[1].Input.ProviderRef = &unboundedv1alpha3.ProviderMachineSnapshot{
				APIGroup: "example.io", Kind: "Host", Name: "forged", UID: "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b", Generation: 1,
			}
		},
		"remove input": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[1].Input = nil
		},
		"add input to a target without either field": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[2].Input = input("forged")
		},
		"add a target with providerOperation": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets = append(status.Targets, unboundedv1alpha3.MachineOperationTargetStatus{MachineRef: "smuggled", ProviderOperation: providerOperation("forged")})
		},
		"add a target with input": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets = append(status.Targets, unboundedv1alpha3.MachineOperationTargetStatus{MachineRef: "smuggled", Input: input("forged")})
		},
		"remove a target that carries the fields": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets = status.Targets[1:]
		},
		"rename a target that carries the fields": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].MachineRef = "renamed"
		},
		"move the fields to another target": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].Input, status.Targets[1].Input = status.Targets[1].Input, status.Targets[0].Input
		},
		"reorder the targets that carry the fields": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0], status.Targets[1] = status.Targets[1], status.Targets[0]
		},
		"remove every target": func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets = nil
		},
	}

	for name, mutate := range denied {
		requireDenied(t, update(agent, "seeded", mutate), providerStateDenial)
		require.NoError(t, update(controller, "seeded", mutate, client.DryRunAll), "controller: %s", name)
	}

	// A patch is an update of the subresource too.
	var patched unboundedv1alpha3.MachineOperation
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: "seeded"}, &patched))

	base := patched.DeepCopy()
	patched.Status.Targets[0].ProviderOperation.ResumeToken = "forged"
	requireDenied(t, agent.Status().Patch(ctx, &patched, client.MergeFrom(base)), providerStateDenial)

	// Nothing above changed the stored state.
	var stored unboundedv1alpha3.MachineOperation
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: "seeded"}, &stored))
	require.Equal(t, providerOperation("token-a"), stored.Status.Targets[0].ProviderOperation)
	require.Equal(t, input("image-a"), stored.Status.Targets[0].Input)
	require.Equal(t, input("image-b"), stored.Status.Targets[1].Input)
	require.Len(t, stored.Status.Targets, 3)

	// The agent still reports everything else on the same operation.
	require.NoError(t, update(agent, "seeded", func(status *unboundedv1alpha3.MachineOperationStatus) {
		now := metav1.Now()

		status.Phase = unboundedv1alpha3.OperationPhaseFailed
		status.Message = "reported by the agent"
		status.CompletedAt = &now
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: "Completed", Status: metav1.ConditionTrue, Reason: "Failed", Message: "reported by the agent"})

		for i := range status.Targets {
			status.Targets[i].Phase = unboundedv1alpha3.OperationPhaseFailed
			status.Targets[i].Message = "reported by the agent"
			status.Targets[i].BootID = "boot-1"
			status.Targets[i].CompletedAt = &now
			apimeta.SetStatusCondition(&status.Targets[i].Conditions, metav1.Condition{Type: "Observed", Status: metav1.ConditionTrue, Reason: "Seen"})
		}
	}))
	require.NoError(t, update(agent, "seeded", func(status *unboundedv1alpha3.MachineOperationStatus) {
		status.Targets = append(status.Targets, unboundedv1alpha3.MachineOperationTargetStatus{MachineRef: "agent-added"})
	}))
	require.NoError(t, update(agent, "seeded", func(status *unboundedv1alpha3.MachineOperationStatus) {
		status.Targets = slices.DeleteFunc(status.Targets, func(target unboundedv1alpha3.MachineOperationTargetStatus) bool {
			return target.MachineRef == "plain" || target.MachineRef == "agent-added"
		})
	}))

	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: "seeded"}, &stored))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, stored.Status.Phase)
	require.Len(t, stored.Status.Targets, 2)
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, stored.Status.Targets[0].Phase)
	require.Equal(t, providerOperation("token-a"), stored.Status.Targets[0].ProviderOperation)

	// The agent's own operations have no status and no such fields.
	createAll(t, admin, &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-run"},
		Spec:       unboundedv1alpha3.MachineOperationSpec{MachineRef: "host-1", OperationKind: unboundedv1alpha3.OperationHostSoftReboot},
	})
	require.NoError(t, update(agent, "agent-run", func(status *unboundedv1alpha3.MachineOperationStatus) {
		status.Phase = unboundedv1alpha3.OperationPhaseInProgress
		status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{MachineRef: "host-1", Phase: unboundedv1alpha3.OperationPhaseInProgress, BootID: "boot-1"}}
	}))
	requireDenied(t, update(agent, "agent-run", func(status *unboundedv1alpha3.MachineOperationStatus) {
		status.Targets[0].ProviderOperation = providerOperation("forged")
	}), providerStateDenial)

	// Status sent with a create is dropped, so it cannot carry the fields.
	created := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "created-with-status"},
		Spec:       unboundedv1alpha3.MachineOperationSpec{MachineRef: "host-1", OperationKind: unboundedv1alpha3.OperationHostReplace},
		Status: unboundedv1alpha3.MachineOperationStatus{
			Targets: []unboundedv1alpha3.MachineOperationTargetStatus{{MachineRef: "host-1", ProviderOperation: providerOperation("forged")}},
		},
	}
	require.NoError(t, clusterAdmin.Create(ctx, created))
	require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: created.Name}, &stored))
	require.Empty(t, stored.Status.Targets)

	// The controllers and cluster-admin write, change and clear the fields.
	for name, writer := range map[string]client.Client{"machine-ops": controller, "metalman": metalman, "cluster-admin": clusterAdmin} {
		operation := "written-by-" + name
		seed(operation)

		require.NoError(t, update(writer, operation, func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation = providerOperation("token-next")
			status.Targets[1].ProviderOperation = providerOperation("token-b")
			status.Targets[2].Input = input("image-c")
			status.Targets = append(status.Targets, unboundedv1alpha3.MachineOperationTargetStatus{MachineRef: "added", Input: input("image-d")})
		}), name)
		require.NoError(t, update(writer, operation, func(status *unboundedv1alpha3.MachineOperationStatus) {
			status.Targets[0].ProviderOperation = nil
			status.Targets = status.Targets[:3]
		}), name)

		require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: operation}, &stored))
		require.Len(t, stored.Status.Targets, 3)
		require.Nil(t, stored.Status.Targets[0].ProviderOperation)
		require.Equal(t, providerOperation("token-b"), stored.Status.Targets[1].ProviderOperation)
		require.Equal(t, input("image-c"), stored.Status.Targets[2].Input)
	}
}

// renderedClusterRoles renders the templates of dir with their default values
// and returns the ClusterRoles by name.
func renderedClusterRoles(t *testing.T, dir string) map[string]*rbacv1.ClusterRole {
	t.Helper()

	outputDir := t.TempDir()
	require.NoError(t, render.Render(dir, outputDir, map[string]string{}))

	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)

	roles := map[string]*rbacv1.ClusterRole{}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		content, err := os.ReadFile(filepath.Join(outputDir, entry.Name()))
		require.NoError(t, err)

		for document := range strings.SplitSeq(string(content), "\n---") {
			var typeMeta metav1.TypeMeta
			require.NoError(t, yaml.Unmarshal([]byte(document), &typeMeta), entry.Name())

			if typeMeta.Kind != "ClusterRole" {
				continue
			}

			role := &rbacv1.ClusterRole{}
			require.NoError(t, yaml.UnmarshalStrict([]byte(document), role), entry.Name())

			roles[role.Name] = role
		}
	}

	return roles
}

// allowsFinalizersUpdate reports whether role is allowed update on
// machineoperations/finalizers, the request the policy sends the authorizer,
// counting the wildcards RBAC honors.
func allowsFinalizersUpdate(role *rbacv1.ClusterRole) bool {
	if role == nil {
		return false
	}

	for _, rule := range role.Rules {
		group := slices.Contains(rule.APIGroups, "unbounded-cloud.io") || slices.Contains(rule.APIGroups, "*")
		resource := slices.Contains(rule.Resources, "machineoperations/finalizers") ||
			slices.Contains(rule.Resources, "*/finalizers") || slices.Contains(rule.Resources, "*")
		verb := slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "*")

		if group && resource && verb && len(rule.ResourceNames) == 0 {
			return true
		}
	}

	return false
}
