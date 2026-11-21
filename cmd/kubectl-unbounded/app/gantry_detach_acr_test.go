// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/registryauth"
)

func detachFixture(t *testing.T) (*detachACRHandler, *fake.Clientset) {
	t.Helper()

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-config", Namespace: "custom-system"},
		Data: map[string]string{
			"config.yaml":          "# retain comment\nupstream_registries:\n- name: ghcr.io\n  endpoint: https://ghcr.io\n",
			registryauth.ConfigKey: "mode: aks-identity-binding\nregistry: central.azurecr.io\n",
		},
	}
	client := fake.NewClientset(source)
	h := &detachACRHandler{
		namespace: source.Namespace, timeout: 5 * time.Second, client: client, out: &bytes.Buffer{},
	}

	return h, client
}

func TestGantryDetachACRRevertsPolicyToExternal(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{"off-azure": true}}

	require.NoError(t, h.execute(t.Context()))

	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)

	cfg, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
	require.NoError(t, err)
	require.Equal(t, registryauth.ExternalMode, cfg.Mode)
}

func TestGantryDetachACRWaitTimesOutWhenNodesUnconverged(t *testing.T) {
	h, _ := detachFixture(t)
	h.timeout = 2 * time.Second

	polls := 0
	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		polls++

		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, detail: "still converging"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
	require.GreaterOrEqual(t, polls, 1)
}

// TestGantryDetachACRDoesNotSucceedOnConfiguredReport verifies the detach wait
// does not complete when a node still reports the managed (Configured) policy:
// a mode-blind reader would have counted a True condition as converged, but the
// structured status rejects a Configured reason for an external detach, so the
// wait retries until the timeout.
func TestGantryDetachACRDoesNotSucceedOnConfiguredReport(t *testing.T) {
	h, _ := detachFixture(t)
	h.timeout = 2 * time.Second

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		// The node still reports the managed reason (not External): a mode-blind
		// reader would have counted this as converged.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, detail: "condition reports Configured, expected External for this policy"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
}

func TestGantryDetachACRRemoveGrantRequiresClusterID(t *testing.T) {
	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = ""

	require.ErrorContains(t, h.execute(t.Context()), "--cluster-resource-id is required with --remove-azure")
}

func TestGantryDetachACRRemoveGrantIsOwnershipGated(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.restConfig = &rest.Config{Host: "https://cluster.example.com"}

	// The preflight validates the kubeconfig against the cluster FQDN, so the
	// fake aks show returns a matching cluster. Model Azure as having no pull
	// identity or binding for this cluster (the UAMI and binding were already
	// removed or never created), so the Azure reverse is a no-op and the test
	// focuses on the K8s RBAC ownership gate.
	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch strings.Join(args[:min(2, len(args))], " ") {
		case "aks show":
			return json.Marshal(map[string]any{"id": h.clusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			return nil, errors.New("Resource 'pull' was not found")
		case "role assignment list", "role assignment delete":
			return []byte("[]"), nil
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	// An unowned grant must be left in place.
	_, err := client.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: registryauth.IdentityRoleName, Labels: map[string]string{"app.kubernetes.io/managed-by": "customer"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.ErrorContains(t, h.execute(t.Context()), "not owned by this installation")

	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "customer", binding.Labels["app.kubernetes.io/managed-by"], "an unowned grant must survive detach")
}

// detachAzureFixture builds a remove-azure detach handler whose Azure fake
// reports a successfully provisioned cluster matching the kubeconfig, so the
// preflight passes and the test can exercise the Azure reverse steps. It returns
// the handler and a pointer to a slice that records the Azure CLI invocations
// issued, so a test can assert which resources were actually attempted.
func detachAzureFixture(t *testing.T) (*detachACRHandler, *[]string) {
	t.Helper()

	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.restConfig = &rest.Config{Host: "https://cluster.example.com"}

	// The AcrPull built-in role ID and a matching ACR scope, for the role
	// assignment reversal cases.
	const acrPullRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	const acrScope = attachACRID

	var calls []string

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))

		switch detachAzureCmd(args) {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			// The owned UAMI exists with the matching tag and a principal.
			return json.Marshal(map[string]any{"principalId": "principal-1", "tags": map[string]string{"unbounded-cluster-id": attachClusterID}})
		case "role assignment list":
			// One ACR pull assignment on the owned principal.
			return json.Marshal([]map[string]any{{
				"principalId":      "principal-1",
				"roleDefinitionId": acrPullRole,
				"scope":            acrScope,
				"id":               "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleAssignments/aaaa",
				"condition":        "",
			}})
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	return h, &calls
}

// detachAzureCmd returns a normalized command key for the first 2-3 Azure CLI
// args, so a switch can match both "aks show" and "role assignment list"
// uniformly.
func detachAzureCmd(args []string) string {
	n := 2
	if len(args) >= 3 && args[0] == "role" && args[1] == "assignment" {
		n = 3
	}

	return strings.Join(args[:min(n, len(args))], " ")
}

// detachAzureBase returns the subset of detachAzureFixture that does not involve
// the role-assignment list, for tests that override runAzure themselves.
func detachAzureBase(t *testing.T) *detachACRHandler {
	t.Helper()

	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.restConfig = &rest.Config{Host: "https://cluster.example.com"}

	return h
}

// TestGantryDetachACRPreflightRejectsMismatchedKubeconfig verifies a remove-azure
// detach refuses before any policy mutation when the selected kubeconfig does
// not target the requested AKS cluster: the preflight is called, and the
// node-auth policy is left untouched.
func TestGantryDetachACRPreflightRejectsMismatchedKubeconfig(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	// A kubeconfig pointing at a different host than the cluster's FQDN.
	h.restConfig = &rest.Config{Host: "https://different.example.com"}

	preflightCalled := false

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "aks" && args[1] == "show" {
			preflightCalled = true

			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		}

		return []byte("{}"), nil
	}

	require.ErrorContains(t, h.execute(t.Context()), "does not match the requested AKS cluster")
	require.True(t, preflightCalled, "a mismatched kubeconfig must fail preflight before any mutation")

	// The policy must still be managed, not reverted to external. The fixture's
	// managed config is not fully validated (it lacks a clientID), so check the
	// raw mode rather than round-tripping through Derive.
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, source.Data[registryauth.ConfigKey], "mode: aks-identity-binding", "a failed preflight must not revert the policy")
}

// TestGantryDetachACRRemovesOwnedACRRoleAssignment verifies the role-assignment
// reversal deletes the ACR pull grant the attach path created on the owned
// principal, and records the delete call.
func TestGantryDetachACRRemovesOwnedACRRoleAssignment(t *testing.T) {
	h, calls := detachAzureFixture(t)

	require.NoError(t, h.execute(t.Context()))

	var deleted bool

	for _, c := range *calls {
		if strings.Contains(c, "role assignment delete") && strings.Contains(c, "roleAssignments/aaaa") {
			deleted = true
		}
	}

	require.True(t, deleted, "the owned ACR pull role assignment must be deleted, not left behind or faked")
}

// TestGantryDetachACRConditionalGrantIsRefused verifies a conditional role
// assignment on the owned principal (one the attach path did not create) is
// refused, not deleted and not faked as success.
func TestGantryDetachACRConditionalGrantIsRefused(t *testing.T) {
	h := detachAzureBase(t)

	const acrPullRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch detachAzureCmd(args) {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			return json.Marshal(map[string]any{"principalId": "principal-1", "tags": map[string]string{"unbounded-cluster-id": attachClusterID}})
		case "role assignment list":
			// A conditional ACR pull grant on the owned principal.
			return json.Marshal([]map[string]any{{
				"principalId":      "principal-1",
				"roleDefinitionId": acrPullRole,
				"scope":            attachACRID,
				"id":               "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleAssignments/bbbb",
				"condition":        `"someCondition":true`,
			}})
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	require.ErrorContains(t, h.execute(t.Context()), "conditional")
}

// TestGantryDetachACRMissingIdentityIsNoOp verifies that when the owned UAMI does
// not exist, the Azure reverse is a no-op success (absence is success) and the
// detach proceeds to revert the policy.
func TestGantryDetachACRMissingIdentityIsNoOp(t *testing.T) {
	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.restConfig = &rest.Config{Host: "https://cluster.example.com"}

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch detachAzureCmd(args) {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			// The specific "resource not found" error: absence, not a failure.
			return nil, errors.New("Resource 'pull' was not found")
		case "role assignment list":
			return []byte("[]"), nil
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	require.NoError(t, h.execute(t.Context()))
}
