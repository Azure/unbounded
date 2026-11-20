// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machina

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/pkg/machineops/fanout"
)

const (
	machinaControllerRole = "machina-controller"
	agentDaemonRole       = "unbounded-daemon-controller-machine"
	agentDaemonGroup      = "unbounded-agent-daemons"
	parentStatusPolicy    = "08-machineoperation-parent-status-policy.yaml"
	parentStatusDenial    = "only an identity allowed to create machineoperations may write the status of a selector operation or of a held operation"
)

// renderedMachina renders the machina templates and returns the directory.
func renderedMachina(t *testing.T) string {
	t.Helper()

	outputDir := t.TempDir()
	require.NoError(t, render.Render(".", outputDir, map[string]string{}))

	return outputDir
}

// renderedDocuments returns the YAML documents of one rendered file.
func renderedDocuments(t *testing.T, file string) []string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(renderedMachina(t), file))
	require.NoError(t, err)

	var documents []string

	for document := range strings.SplitSeq(string(content), "\n---") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}

	return documents
}

// renderedClusterRole returns one ClusterRole of a rendered file.
func renderedClusterRole(t *testing.T, file, name string) *rbacv1.ClusterRole {
	t.Helper()

	for _, document := range renderedDocuments(t, file) {
		var typeMeta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(document), &typeMeta))

		if typeMeta.Kind != "ClusterRole" {
			continue
		}

		role := &rbacv1.ClusterRole{}
		require.NoError(t, yaml.UnmarshalStrict([]byte(document), role))

		if role.Name == name {
			return role
		}
	}

	t.Fatalf("no ClusterRole %s in %s", name, file)

	return nil
}

func machinaClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()

	return renderedClusterRole(t, "02-rbac.yaml", machinaControllerRole)
}

func agentDaemonClusterRole(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()

	return renderedClusterRole(t, "07-bootstrapper-rbac.yaml", agentDaemonRole)
}

// applyParentStatusPolicy installs the rendered admission policy and binding.
func applyParentStatusPolicy(t *testing.T, admin client.Client) {
	t.Helper()

	for _, document := range renderedDocuments(t, parentStatusPolicy) {
		object := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal([]byte(document), &object.Object))
		require.NoError(t, admin.Create(t.Context(), object), "apply %s %s", object.GetKind(), object.GetName())
	}
}

// requireAgentCannotWriteTheParent tries, as an agent, the status writes that
// would mislead an operator or steer the fan-out controller: forging the
// counts, and ending the parent.
// A policy takes a moment to be enforced after it is created, so the first
// denial is waited for.
func requireAgentCannotWriteTheParent(t *testing.T, ctx context.Context, agent client.Client, parentName string) {
	t.Helper()

	forgeCounts := func() error {
		var parent unboundedv1alpha3.MachineOperation
		if err := agent.Get(ctx, client.ObjectKey{Name: parentName}, &parent); err != nil {
			return err
		}

		parent.Status.Summary = &unboundedv1alpha3.MachineOperationSummary{Total: 2, Complete: 2}

		return agent.Status().Update(ctx, &parent)
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		err := forgeCounts()
		require.Error(c, err)
		require.True(c, apierrors.IsForbidden(err), "%v", err)
		require.Contains(c, err.Error(), parentStatusDenial)
	}, 30*time.Second, 200*time.Millisecond)

	var parent unboundedv1alpha3.MachineOperation
	require.NoError(t, agent.Get(ctx, client.ObjectKey{Name: parentName}, &parent))

	parent.Status.Phase = unboundedv1alpha3.OperationPhaseComplete

	err := agent.Status().Update(ctx, &parent)
	require.Error(t, err)
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	require.Contains(t, err.Error(), parentStatusDenial)
}

// The policy must cover exactly the status subresource of MachineOperations,
// fail closed, and be bound with Deny.
func TestParentStatusPolicyShape(t *testing.T) {
	t.Parallel()

	documents := renderedDocuments(t, parentStatusPolicy)
	require.Len(t, documents, 2)

	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.UnmarshalStrict([]byte(documents[0]), &policy))

	require.NotNil(t, policy.Spec.FailurePolicy)
	require.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
	require.Len(t, policy.Spec.MatchConstraints.ResourceRules, 1)

	rule := policy.Spec.MatchConstraints.ResourceRules[0]
	require.Equal(t, []string{"unbounded-cloud.io"}, rule.APIGroups)
	require.Equal(t, []string{"machineoperations/status"}, rule.Resources)
	require.Equal(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Update}, rule.Operations)

	require.Len(t, policy.Spec.Validations, 1)
	require.Equal(t, parentStatusDenial, policy.Spec.Validations[0].Message)

	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.UnmarshalStrict([]byte(documents[1]), &binding))
	require.Equal(t, policy.Name, binding.Spec.PolicyName)
	require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
}

// The policy tells the fan-out controller from an agent by the right to
// create MachineOperations, so the shipped roles must keep that split.
func TestCreateGrantSeparatesTheFanOutFromTheAgents(t *testing.T) {
	t.Parallel()

	require.Contains(t, verbsOn(machinaClusterRole(t), "machineoperations"), "create")

	// An agent reads operations and reports on them. It may not create one,
	// release one (a patch of spec.suspend) or delete one: a selector
	// operation counts a deleted child as failed.
	require.Equal(t, []string{"get", "list", "watch"}, verbsOn(agentDaemonClusterRole(t), "machineoperations"))
	require.Contains(t, verbsOn(agentDaemonClusterRole(t), "machineoperations/status"), "update")
}

// verbsOn returns the sorted verbs the role grants on one resource of the
// unbounded-cloud.io group.
func verbsOn(role *rbacv1.ClusterRole, resource string) []string {
	var verbs []string

	for _, rule := range role.Rules {
		if !slices.Contains(rule.APIGroups, "unbounded-cloud.io") || !slices.Contains(rule.Resources, resource) {
			continue
		}

		for _, verb := range rule.Verbs {
			if !slices.Contains(verbs, verb) {
				verbs = append(verbs, verb)
			}
		}
	}

	slices.Sort(verbs)

	return verbs
}

// The fan-out controller creates and deletes MachineOperations and writes
// their status. It must not be able to update them, and it must not hold
// update on machineoperations/finalizers: that grant is how the provider
// state admission policy recognizes a controller that may write a target's
// providerOperation and input.
func TestMachinaRoleGrantsTheFanOutExactly(t *testing.T) {
	t.Parallel()

	role := machinaClusterRole(t)

	require.Equal(t, []string{"create", "delete", "get", "list", "patch", "watch"}, verbsOn(role, "machineoperations"))
	require.Equal(t, []string{"get", "update"}, verbsOn(role, "machineoperations/status"))
	require.Empty(t, verbsOn(role, "machineoperations/finalizers"))

	for _, verb := range []string{"get", "list", "watch"} {
		require.Contains(t, verbsOn(role, "machines"), verb)
	}
}

// With only the rendered machina role, the fan-out controller takes a
// selector operation from creation to Complete.
func TestEnvtestFanOutRunsUnderTheMachinaRole(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API server tests")
	}

	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"crd"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, unboundedv1alpha3.AddToScheme(scheme))

	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	ctx := t.Context()

	role := machinaClusterRole(t)
	require.NoError(t, admin.Create(ctx, role))
	applyParentStatusPolicy(t, admin)

	agentRole := agentDaemonClusterRole(t)
	require.NoError(t, admin.Create(ctx, agentRole))
	require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agents"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: agentRole.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: agentDaemonGroup}},
	}))

	agentUser, err := env.AddUser(envtest.User{Name: "system:node:machine-a", Groups: []string{agentDaemonGroup}}, nil)
	require.NoError(t, err)

	agent, err := client.New(agentUser.Config(), client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machina"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: machinaControllerRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "machina"}},
	}))

	user, err := env.AddUser(envtest.User{Name: "machina"}, nil)
	require.NoError(t, err)

	mgr, err := ctrl.NewManager(user.Config(), ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	require.NoError(t, err)
	require.NoError(t, fanout.AddToManager(mgr, fanout.Options{MinReconcileInterval: 50 * time.Millisecond}))

	managerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)

	go func() { done <- mgr.Start(managerCtx) }()

	t.Cleanup(func() {
		stop()
		require.NoError(t, <-done)
	})

	rack := map[string]string{"topology.unbounded-cloud.io/rack": "dc-a.rack-01"}
	for _, name := range []string{"machine-a", "machine-b"} {
		require.NoError(t, admin.Create(ctx, &unboundedv1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: rack},
		}))
	}

	parent := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "reboot-rack"},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineSelector: &metav1.LabelSelector{MatchLabels: rack},
			OperationKind:   unboundedv1alpha3.OperationNodeReboot,
		},
	}
	require.NoError(t, admin.Create(ctx, parent))

	// Both children exist from the start, the second one held: the default
	// pace is one at a time. The agents run them: each child is completed,
	// by an agent identity, once the controller has released it.
	for i, machine := range []string{"machine-a", "machine-b"} {
		var child unboundedv1alpha3.MachineOperation

		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var children unboundedv1alpha3.MachineOperationList
			require.NoError(c, admin.List(ctx, &children, client.MatchingFields{unboundedv1alpha3.MachineOperationMachineRefField: machine}))
			require.Len(c, children.Items, 1)
			require.False(c, children.Items[0].Spec.Suspend, "child for %s is still held", machine)

			child = children.Items[0]
		}, 30*time.Second, 100*time.Millisecond, "child for %s", machine)

		if i == 0 {
			var second unboundedv1alpha3.MachineOperationList
			require.NoError(t, admin.List(ctx, &second, client.MatchingFields{unboundedv1alpha3.MachineOperationMachineRefField: "machine-b"}))
			require.Len(t, second.Items, 1)
			require.True(t, second.Items[0].Spec.Suspend, "the second child is held while the first runs")

			requireAgentCannotWriteTheParent(t, ctx, agent, parent.Name)

			// Nor the held child: a forged result on it would be believed
			// when its turn came.
			held := second.Items[0].DeepCopy()
			held.Status.Phase = unboundedv1alpha3.OperationPhaseComplete

			err := agent.Status().Update(ctx, held)
			require.Error(t, err)
			require.True(t, apierrors.IsForbidden(err), "%v", err)
			require.Contains(t, err.Error(), parentStatusDenial)

			require.Error(t, agent.Delete(ctx, held), "an agent may not delete an operation")
		}

		now := metav1.Now()
		child.Status.Phase = unboundedv1alpha3.OperationPhaseComplete
		child.Status.CompletedAt = &now
		require.NoError(t, agent.Status().Update(ctx, &child), "an agent reports its own machineRef operation")
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var current unboundedv1alpha3.MachineOperation
		require.NoError(c, admin.Get(ctx, client.ObjectKey{Name: parent.Name}, &current))
		require.Equal(c, unboundedv1alpha3.OperationPhaseComplete, current.Status.Phase)
		require.NotNil(c, current.Status.Summary)
		require.Equal(c, int32(2), current.Status.Summary.Total)
		require.Equal(c, int32(2), current.Status.Summary.Complete)
		require.Empty(c, current.Status.Targets)
	}, 30*time.Second, 100*time.Millisecond)

	// The role is enough to remove the completed children and to say so on
	// the finished parent, which the status policy allows the controller.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var children unboundedv1alpha3.MachineOperationList
		require.NoError(c, admin.List(ctx, &children,
			client.MatchingLabels{unboundedv1alpha3.MachineOperationParentUIDLabel: string(parent.UID)}))
		require.Empty(c, children.Items)

		var current unboundedv1alpha3.MachineOperation
		require.NoError(c, admin.Get(ctx, client.ObjectKey{Name: parent.Name}, &current))
		require.True(c, apimeta.IsStatusConditionTrue(current.Status.Conditions,
			unboundedv1alpha3.MachineOperationConditionCompletedChildrenRemoved))
		require.Equal(c, int32(2), current.Status.Summary.Complete)
	}, 30*time.Second, 100*time.Millisecond)
}
