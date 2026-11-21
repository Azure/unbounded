// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const acrUpstream = "upstream_registries:\n  - name: example.azurecr.io\n    endpoint: https://example.azurecr.io\n"

func authSource(policy string) *corev1.ConfigMap {
	if !strings.HasPrefix(policy, "mode:") {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}

		template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}

		cert, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
		if err != nil {
			panic(err)
		}

		cfg := registryauth.Config{
			Mode: registryauth.IdentityBindingMode, Registry: "example.azurecr.io",
			ClientID: "11111111-2222-3333-4444-555555555555", TenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			SNIName: "proxy.example.com", ProxyAddress: "10.0.0.1", Audience: "api://AzureADTokenExchange",
			CACert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})),
		}

		data, err := yaml.Marshal(cfg)
		if err != nil {
			panic(err)
		}

		if policy != "" && policy != "registry: example.azurecr.io" {
			panic("unsupported test policy")
		}

		policy = string(data)
	}

	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace}, Data: map[string]string{"config.yaml": acrUpstream, registryauth.ConfigKey: policy}}
}

// registryAuthFlexNodeMachine seeds an eligible Linux AKS Flex node and its
// Machine, so planRegistryAuth has an agent-backed node to deliver the policy
// to. The Machine's reported condition can be set to model agent readiness.
func registryAuthFlexNodeMachine(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue,
	}}}
}

func registryAuthMachine(name string, spec *unboundedv1alpha3.RegistryAuthPolicy, cond *metav1.Condition) *unboundedv1alpha3.Machine {
	m := &unboundedv1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	m.Spec.RegistryAuth = spec

	if cond != nil {
		m.Status.Conditions = []metav1.Condition{*cond}
	}

	return m
}

func registryAuthHealthyCondition() *metav1.Condition {
	return &metav1.Condition{Type: unboundedv1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue, Reason: "Configured", ObservedGeneration: 1}
}

// registryAuthExternalCondition models a node that reports the external policy
// converged (no managed provider on the kubelet).
func registryAuthExternalCondition() *metav1.Condition {
	return &metav1.Condition{Type: unboundedv1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue, Reason: "External", ObservedGeneration: 1}
}

// managedRegistryAuthPolicy derives the Machine.spec.registryAuth the operator
// delivers for a given source, so a test can model a node whose delivered spec
// matches the desired policy (the readiness contract requires the spec to
// match, not merely the condition to be True).
func managedRegistryAuthPolicy(t *testing.T, source *corev1.ConfigMap) *unboundedv1alpha3.RegistryAuthPolicy {
	t.Helper()

	cfg, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
	require.NoError(t, err)

	return registryAuthPolicyFromConfig(cfg)
}

func TestRegistryAuthDeliversPolicyToAgentBackedNodes(t *testing.T) {
	source := authSource("")
	node := registryAuthFlexNodeMachine("flex-a")
	machine := registryAuthMachine("flex-a", nil, nil)
	env := testEnv(t, source, node, machine)

	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.NotNil(t, result)
	// The node has not reported its state yet, so the operator is installing.
	require.False(t, result.Ready)
	require.Equal(t, "RegistryAuthInstalling", result.Reason)

	plan := component.NewPlan()
	plan.Add(operations...)
	executed, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.Empty(t, executed.Failed())
	require.Empty(t, executed.Deferred)

	// The policy is now on the Machine spec, where the agent reads it.
	var got unboundedv1alpha3.Machine
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "flex-a"}, &got))
	require.NotNil(t, got.Spec.RegistryAuth)
	require.Equal(t, registryauth.IdentityBindingMode, got.Spec.RegistryAuth.Mode)
	require.Equal(t, "example.azurecr.io", got.Spec.RegistryAuth.Registry)
}

func TestRegistryAuthReadyWhenNodesReportHealthy(t *testing.T) {
	source := authSource("")
	node := registryAuthFlexNodeMachine("flex-a")
	// The node reports healthy AND the delivered spec matches the desired
	// policy, so readiness is satisfied (not merely a True condition).
	machine := registryAuthMachine("flex-a", managedRegistryAuthPolicy(t, source), registryAuthHealthyCondition())
	env := testEnv(t, source, node, machine)

	_, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.True(t, result.Ready)
	require.Equal(t, component.ReasonReconciled, result.Reason)
	require.Contains(t, result.Message, "verified")
}

func TestRegistryAuthFailedWhenANodeReportsFailure(t *testing.T) {
	source := authSource("")
	node := registryAuthFlexNodeMachine("flex-a")
	cond := &metav1.Condition{Type: unboundedv1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionFalse, Reason: "RestartBudgetExhausted", Message: "budget exhausted"}
	// The spec must match the desired policy for the failure to be terminal:
	// a budget exhaustion for a policy that has actually been delivered is a
	// genuine failure; one for an undelivered (nil) spec is still installing.
	machine := registryAuthMachine("flex-a", managedRegistryAuthPolicy(t, source), cond)
	env := testEnv(t, source, node, machine)

	_, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.False(t, result.Ready)
	require.Equal(t, "RegistryAuthFailed", result.Reason)
	require.Contains(t, result.Message, "flex-a")
}

// TestRegistryAuthStaleSpecFailureIsInstalling verifies that a
// RestartBudgetExhausted condition for a policy NOT delivered on the spec
// (the spec is nil, meaning the operator has not yet patched the new policy)
// is reported as installing, not failed: the failure is for the old (absent)
// policy, not the one being planned.
func TestRegistryAuthStaleSpecFailureIsInstalling(t *testing.T) {
	source := authSource("")
	node := registryAuthFlexNodeMachine("flex-a")
	cond := &metav1.Condition{Type: unboundedv1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionFalse, Reason: "RestartBudgetExhausted", Message: "budget exhausted for old policy"}
	// The spec is nil: the desired policy has not been delivered yet.
	machine := registryAuthMachine("flex-a", nil, cond)
	env := testEnv(t, source, node, machine)

	_, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.False(t, result.Ready)
	require.Equal(t, "RegistryAuthInstalling", result.Reason)
}

func TestRegistryAuthExternalClearsManagedPolicy(t *testing.T) {
	source := authSource("mode: external")
	activePolicy := &unboundedv1alpha3.RegistryAuthPolicy{Mode: registryauth.IdentityBindingMode, Registry: "example.azurecr.io"}
	node := registryAuthFlexNodeMachine("flex-a")
	machine := registryAuthMachine("flex-a", activePolicy, registryAuthHealthyCondition())
	env := testEnv(t, source, node, machine)

	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.NotEmpty(t, operations, "external mode must clear the active policy on the Machine")

	plan := component.NewPlan()
	plan.Add(operations...)
	_, err = env.Execute(t.Context(), plan)
	require.NoError(t, err)

	var got unboundedv1alpha3.Machine
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "flex-a"}, &got))
	require.Nil(t, got.Spec.RegistryAuth, "external mode leaves the kubelet's authentication untouched")
}

func TestRegistryAuthRetiresLegacyInstaller(t *testing.T) {
	source := authSource("mode: external")
	node := registryAuthFlexNodeMachine("flex-a")
	// External mode: the desired policy is nil, the machine's spec is nil (a
	// match), and the node reports the external condition, so readiness holds.
	machine := registryAuthMachine("flex-a", nil, registryAuthExternalCondition())
	legacyDaemonSet := &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()}
	legacyAccount := &corev1.ServiceAccount{ObjectMeta: ownedAuthMetadata()}
	legacyMarker := &corev1.ConfigMap{ObjectMeta: ownedAuthMetadata()}
	env := testEnv(t, source, node, machine, legacyDaemonSet, legacyAccount, legacyMarker)

	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.True(t, result.Ready)

	// The owned legacy objects are retired (deleted).
	kinds := map[string]bool{}

	for _, op := range operations {
		if op.Kind == component.OpDelete {
			kinds[op.Object.GetKind()] = true
		}
	}

	require.True(t, kinds["DaemonSet"], "the legacy installer DaemonSet is retired")
	require.True(t, kinds["ServiceAccount"], "the legacy ServiceAccount is retired")
	require.True(t, kinds["ConfigMap"], "the legacy ConfigMap marker is retired")
}

func TestRegistryAuthNoFlexNodesIsReady(t *testing.T) {
	source := authSource("")
	env := testEnv(t, source)

	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.Empty(t, operations)
	require.True(t, result.Ready)
	require.Contains(t, result.Message, "no agent-backed Flex nodes")
}

func TestExternalCleanupRevokesOnlyOwnedIdentityUseBinding(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: registryauth.IdentityRoleName}}
			if owned {
				binding.Labels = map[string]string{"app.kubernetes.io/managed-by": acrAttachManagerForTest, "unbounded-cloud.io/namespace": component.DefaultNamespace}
			}

			env := testEnv(t, authSource("mode: external"), &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()})
			require.NoError(t, rbacv1.AddToScheme(env.Client.Scheme()))
			require.NoError(t, env.Client.Create(t.Context(), binding))

			operations, err := planRegistryAuthAudience(t.Context(), env, registryauth.Config{Mode: registryauth.ExternalMode})
			if !owned {
				require.ErrorContains(t, err, "unowned")
				return
			}

			require.NoError(t, err)
			require.Equal(t, component.OpDelete, operations[0].Kind)
			require.Equal(t, "ClusterRoleBinding", operations[0].Object.GetKind())
			require.Equal(t, registryauth.IdentityRoleName, operations[0].Object.GetName())
		})
	}
}

const acrAttachManagerForTest = "unbounded-acr-attach"

func ownedAuthMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: registryAuthName, Namespace: component.DefaultNamespace, UID: "owned-installer",
		Labels: map[string]string{"app.kubernetes.io/name": registryAuthName, "app.kubernetes.io/managed-by": installationManagerOperator},
	}
}

// countingClient wraps a client.Client and counts Get and List calls, so a
// 10k-node test can assert the registry-auth plan is bounded (a single List of
// the watched kind, not a live Get per node).
type countingClient struct {
	client.Client
	gets  int
	lists int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++

	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++

	return c.Client.List(ctx, list, opts...)
}

// TestRegistryAuthBoundedAt10kNodes asserts the registry-auth plan observes the
// fleet with a single bounded List of Machines plus the node membership List,
// and does no per-node live read. This is the 10k-node contract: an unchanged
// fleet must not fan out N apiserver calls as the node count grows.
func TestRegistryAuthBoundedAt10kNodes(t *testing.T) {
	const nodes = 10_000

	source := authSource("")

	objs := make([]client.Object, 0, nodes*2)

	for i := 0; i < nodes; i++ {
		name := fmt.Sprintf("flex-%05d", i)
		objs = append(objs,
			registryAuthFlexNodeMachine(name),
			registryAuthMachine(name, nil, registryAuthHealthyCondition()),
		)
	}

	env := testEnv(t, source)
	for _, obj := range objs {
		require.NoError(t, env.Client.Create(t.Context(), obj))
	}

	client_ := env.Client
	reader := &countingClient{Client: client_}
	env.Client = reader
	// The registry-auth plan reads only through the cached client; a nil
	// APIReader makes LiveReader fall back to the cached client, but the plan
	// no longer calls it at all.

	policy := &unboundedv1alpha3.RegistryAuthPolicy{Mode: registryauth.IdentityBindingMode}
	operations, machines, err := planRegistryAuthMachines(t.Context(), env, policy, true)
	require.NoError(t, err)
	require.Len(t, machines, nodes)

	// Bounded observation: the node membership List plus the single Machine
	// List. No per-node Get, so the cost is O(1) reads, not O(nodes).
	require.LessOrEqual(t, reader.gets, 1, "no per-node live Get at 10k scale")
	require.LessOrEqual(t, reader.lists, 2, "one node membership List and one Machine List at most")

	// An undelivered fleet plans one patch per Machine (the policy is not yet on
	// the spec); the bound is on the observation above, not the patch count.
	require.Len(t, operations, nodes)
}

func TestInvalidAuthPolicyStopsPlanning(t *testing.T) {
	env := testEnv(t, authSource("mode: unsupported"))
	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("flex", nil)})
	require.ErrorContains(t, err, "mode must")
	require.Nil(t, plan)
}

func TestRegistryAuthAudienceIsLimitedToLinuxFlexNodeUsers(t *testing.T) {
	node := func(name, os, pool string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"kubernetes.io/os": os, registryauth.FlexNodeLabel: pool,
		}}}
	}
	flex := node("flex-a", "linux", registryauth.FlexNodeValue)
	env := testEnv(t, flex, node("native", "linux", "VirtualMachineScaleSets"),
		node("windows", "windows", registryauth.FlexNodeValue))
	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
	check := func(expected ...string) {
		t.Helper()
		operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
		require.NoError(t, err)
		storeAudienceOperations(t, env, operations)
		assignments := audienceAssignments(t, env)
		require.Len(t, assignments, len(expected))

		for _, name := range expected {
			require.Contains(t, assignments, name)
		}
	}
	check("system:node:flex-a")

	var role rbacv1.ClusterRole
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: registryAuthAudienceName}, &role))
	require.Len(t, role.Rules, 1)
	require.Equal(t, []string{cfg.Audience}, role.Rules[0].Resources)

	future := node("flex-b", "linux", registryauth.FlexNodeValue)
	require.NoError(t, env.Client.Create(t.Context(), future))
	check("system:node:flex-a", "system:node:flex-b")

	flex.Labels[registryauth.FlexNodeLabel] = "VirtualMachineScaleSets"
	require.NoError(t, env.Client.Update(t.Context(), flex))
	check("system:node:flex-b")
	require.NoError(t, env.Client.Delete(t.Context(), future))
	check()
}

func TestRegistryAuthAudienceOwnershipAndRevocation(t *testing.T) {
	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}

	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			env := testEnv(t)
			operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
			require.NoError(t, err)

			for _, op := range operations {
				object := op.Object.DeepCopy()
				if foreign {
					object.SetLabels(nil)
				}

				object.SetUID("existing-audience")
				require.NoError(t, env.Client.Create(t.Context(), object))
			}

			changed := cfg
			changed.Audience += "-changed"

			operations, err = planRegistryAuthAudience(t.Context(), env, changed)
			if foreign {
				require.ErrorContains(t, err, "unowned registry-auth")
			} else {
				require.NoError(t, err)
				require.Len(t, operations, 1)

				for _, op := range operations {
					require.Equal(t, component.OpApply, op.Kind)
					require.Equal(t, "existing-audience", string(op.Object.GetUID()))
					require.NotEmpty(t, op.Object.GetResourceVersion())
				}
			}

			operations, err = planRegistryAuthAudience(t.Context(), env, registryauth.Config{Mode: registryauth.ExternalMode})
			require.NoError(t, err)

			if foreign {
				require.Empty(t, operations)
			} else {
				require.Len(t, operations, 2)

				for _, op := range operations {
					require.Equal(t, component.OpDelete, op.Kind)
					require.Equal(t, "existing-audience", string(op.Object.GetUID()))
					require.NotEmpty(t, op.Object.GetResourceVersion())
				}
			}
		})
	}
}
