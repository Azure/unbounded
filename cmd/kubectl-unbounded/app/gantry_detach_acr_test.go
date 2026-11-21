// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// TestGantryDetachACRDoesNotSucceedOnMissingMachine verifies the detach wait
// does not complete when an eligible node has no agent-backed Machine yet: a
// hasMachine=false, ready=false status is an eligible unverified node, not a
// converged one, so it is retried until the timeout (a wait that only checked
// hasMachine && !ready would skip it and declare success immediately).
func TestGantryDetachACRDoesNotSucceedOnMissingMachine(t *testing.T) {
	h, _ := detachFixture(t)
	h.timeout = 2 * time.Second

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		// A node with no Machine yet: the reader retains it as eligible but
		// unverified.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: false, ready: false, detail: "no agent-backed Machine registered yet"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
}

// TestGantryDetachACRTerminalFailureSurfacesImmediately verifies a node that
// reports a terminal (False) condition during detach is surfaced immediately
// rather than polled to the timeout.
func TestGantryDetachACRTerminalFailureSurfacesImmediately(t *testing.T) {
	h, _ := detachFixture(t)
	h.timeout = 30 * time.Second

	start := time.Now()

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, terminal: true, detail: "condition False: RestartBudgetExhausted (budget spent)"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "terminal failure")
	require.WithinDuration(t, time.Now(), start, 3*time.Second, "a terminal failure must not be polled to the timeout")
}

func TestGantryDetachACRRemoveGrantRequiresClusterID(t *testing.T) {
	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = ""

	require.ErrorContains(t, h.execute(t.Context()), "--cluster-resource-id is required with --remove-azure")
}

// TestGantryDetachACRRemoveGrantRequiresACRID verifies a missing
// --acr-resource-id is refused BEFORE any policy mutation: the node-auth
// ConfigMap must still carry the managed policy (not have been rewritten to
// external) when the flag error is returned.
func TestGantryDetachACRRemoveGrantRequiresACRID(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	// ACR ID intentionally left empty.

	require.ErrorContains(t, h.execute(t.Context()), "--acr-resource-id is required with --remove-azure")

	// The policy must not have been rewritten to external before the refusal:
	// the node-auth policy key must still carry the managed mode, not external.
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.Contains(t, source.Data[registryauth.ConfigKey], registryauth.IdentityBindingMode, "a missing ACR ID must be refused before the policy is rewritten")
	require.NotContains(t, source.Data[registryauth.ConfigKey], "\nmode: external", "the policy must not be rewritten to external before the refusal")
}

func TestGantryDetachACRRemoveGrantIsOwnershipGated(t *testing.T) {
	h, client := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.acrID = attachACRID
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

	// An unowned grant must be left in place. The K8s ownership gate runs BEFORE
	// any Azure deletion, so the refusal happens before a role assignment is
	// deleted.
	_, err := client.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: registryauth.IdentityRoleName, Labels: map[string]string{"app.kubernetes.io/managed-by": "customer"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.ErrorContains(t, h.execute(t.Context()), "not owned by this installation")

	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "customer", binding.Labels["app.kubernetes.io/managed-by"], "an unowned grant must survive detach")
}

// TestGantryDetachACRForeignBindingIsPreflighted verifies the AKS identity
// binding's target is validated read-only BEFORE any deletion: a same-name
// binding that references a foreign identity is refused before the owned role
// assignment is deleted, so the detach never deletes some resources and then
// refuse on the rest.
func TestGantryDetachACRForeignBindingIsPreflighted(t *testing.T) {
	h := detachAzureBase(t)

	const acrPullRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	const foreignIdentityID = "/subscriptions/99999999-2222-3333-4444-555555555555/resourceGroups/other-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/other"

	// The deterministic UAMI name the attach path derives from the cluster ID.
	digest := sha256.Sum256([]byte(strings.ToLower(attachClusterID)))
	uamiName := "gantry-pull-" + hex.EncodeToString(digest[:6])

	var deleteCalls []string

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := detachAzureCmd(args)
		if key == "role assignment delete" {
			deleteCalls = append(deleteCalls, strings.Join(args, " "))
		}

		switch key {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			// The owned UAMI exists with the matching tag, a principal, and a
			// resource ID.
			return json.Marshal(map[string]any{
				"id":          attachIdentityID,
				"principalId": "principal-1",
				"tags":        map[string]string{"unbounded-cluster-id": attachClusterID},
			})
		case "role assignment list":
			// An owned ACR pull assignment exists, so a naive implementation would
			// delete it before noticing the foreign binding.
			return json.Marshal([]map[string]any{{
				"name":             acrPullGrantName(attachClusterID, attachACRID, "principal-1", "7f951dda-4ed3-4680-a7ca-43fe172d538d"),
				"principalId":      "principal-1",
				"roleDefinitionId": acrPullRole,
				"scope":            attachACRID,
				"id":               "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleAssignments/aaaa",
				"condition":        "",
			}})
		case "rest --method":
			// A same-name identity binding that references a FOREIGN identity
			// (not the owned UAMI): this must be refused by preflight. The
			// GET lists the bindings; a DELETE (also "rest --method") is never
			// reached because preflight refuses first.
			if len(args) >= 3 && args[2] == "delete" {
				deleteCalls = append(deleteCalls, strings.Join(args, " "))

				return []byte("{}"), nil
			}

			return json.Marshal(map[string]any{"value": []map[string]any{{
				"name": uamiName, "properties": map[string]any{
					"managedIdentity": map[string]any{"resourceId": foreignIdentityID},
				},
			}}})
		}

		return []byte("{}"), nil
	}

	require.ErrorContains(t, h.execute(t.Context()), "references another identity")
	// The foreign binding refusal must happen before any role assignment is
	// deleted.
	require.Empty(t, deleteCalls, "the foreign binding must be refused before a role assignment is deleted")
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
	h.acrID = attachACRID
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
			// One owned ACR pull assignment on the owned principal, carrying the
			// deterministic ownership-receipt name (the role-assignment API has
			// no tags property, so the receipt is the assignment's name). The
			// principal must match the identity show response ("principal-1").
			return json.Marshal([]map[string]any{{
				"name":             acrPullGrantName(attachClusterID, attachACRID, "principal-1", "7f951dda-4ed3-4680-a7ca-43fe172d538d"),
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
	h.acrID = attachACRID
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
	h.acrID = attachACRID
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

// TestGantryDetachACRConditionalGrantIsPreserved verifies a conditional role
// assignment on the owned principal (one the attach path did not create) is
// preserved, not deleted: it is a shared/inherited grant, and the detach
// reports it for manual cleanup rather than deleting it or faking success.
func TestGantryDetachACRConditionalGrantIsPreserved(t *testing.T) {
	h := detachAzureBase(t)

	const acrPullRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	var deleteCalls []string

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := detachAzureCmd(args)
		if key == "role assignment delete" {
			deleteCalls = append(deleteCalls, strings.Join(args, " "))
		}

		switch key {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			return json.Marshal(map[string]any{"principalId": "principal-1", "tags": map[string]string{"unbounded-cluster-id": attachClusterID}})
		case "role assignment list":
			// A conditional ACR pull grant on the owned principal (shared, not
			// created by this installation).
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

	// The conditional grant is preserved: the detach succeeds but does NOT
	// delete it.
	require.NoError(t, h.execute(t.Context()))
	require.Empty(t, deleteCalls, "a conditional (shared) grant must not be deleted")
}

// TestGantryDetachACRPreservesForeignGrant verifies that a role assignment on
// the owned principal that scopes the ACR role but carries a name this
// installation did NOT create (a customer grant the attach path merely reused,
// or a shared/inherited grant) is preserved, not deleted: matching principal +
// ACR scope + unconditional AcrPull is not ownership proof, so only the
// deterministic receipt name is reversed.
func TestGantryDetachACRPreservesForeignGrant(t *testing.T) {
	h := detachAzureBase(t)

	const acrPullRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	var deleteCalls []string

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := detachAzureCmd(args)
		if key == "role assignment delete" {
			deleteCalls = append(deleteCalls, strings.Join(args, " "))
		}

		switch key {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			return json.Marshal(map[string]any{"principalId": "principal-1", "tags": map[string]string{"unbounded-cluster-id": attachClusterID}})
		case "role assignment list":
			// A customer grant: principal + ACR scope + unconditional AcrPull,
			// but a name this installation did not create. The attach path may
			// have reused it.
			return json.Marshal([]map[string]any{{
				"name":             "customer-owned-assignment",
				"principalId":      "principal-1",
				"roleDefinitionId": acrPullRole,
				"scope":            attachACRID,
				"id":               "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleAssignments/customer",
				"condition":        "",
			}})
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	// The foreign-named (customer) grant is preserved: the detach succeeds but
	// does NOT delete it.
	require.NoError(t, h.execute(t.Context()))
	require.Empty(t, deleteCalls, "a foreign-named (customer/shared) grant must not be deleted")
}

// TestGantryDetachACRRemovesRepositoryReaderGrant verifies the Repository Reader
// (ABAC) grant is matched by its published built-in role definition GUID
// (b93aa761-3e63-49ed-ac28-beffa264f7ac), not by a name suffix, and is reversed
// when it carries the unbounded receipt tag.
func TestGantryDetachACRRemovesRepositoryReaderGrant(t *testing.T) {
	h := detachAzureBase(t)

	const repoReaderRole = "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleDefinitions/b93aa761-3e63-49ed-ac28-beffa264f7ac"

	var deleteCalls []string

	h.runAzure = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := detachAzureCmd(args)
		if key == "role assignment delete" {
			deleteCalls = append(deleteCalls, strings.Join(args, " "))
		}

		switch key {
		case "aks show":
			return json.Marshal(map[string]any{"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded"})
		case "identity show":
			return json.Marshal(map[string]any{"principalId": "principal-1", "tags": map[string]string{"unbounded-cluster-id": attachClusterID}})
		case "role assignment list":
			// An owned Repository Reader grant, matched by its built-in GUID and
			// the deterministic ownership-receipt name. The principal must match
			// the identity show response ("principal-1").
			return json.Marshal([]map[string]any{{
				"name":             acrPullGrantName(attachClusterID, attachACRID, "principal-1", "b93aa761-3e63-49ed-ac28-beffa264f7ac"),
				"principalId":      "principal-1",
				"roleDefinitionId": repoReaderRole,
				"scope":            attachACRID,
				"id":               "/subscriptions/11111111-2222-3333-4444-555555555555/providers/Microsoft.Authorization/roleAssignments/reader",
				"condition":        "",
			}})
		case "rest":
			return []byte(`{"value":[]}`), nil
		}

		return []byte("{}"), nil
	}

	require.NoError(t, h.execute(t.Context()))

	var deleted bool

	for _, c := range deleteCalls {
		if strings.Contains(c, "roleAssignments/reader") {
			deleted = true
		}
	}

	require.True(t, deleted, "the owned Repository Reader grant must be deleted by its built-in role GUID")
}

// TestGantryDetachACRMissingIdentityIsNoOp verifies that when the owned UAMI does
// not exist, the Azure reverse is a no-op success (absence is success) and the
// detach proceeds to revert the policy.
func TestGantryDetachACRMissingIdentityIsNoOp(t *testing.T) {
	h, _ := detachFixture(t)
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{}}
	h.removeAzure = true
	h.clusterID = attachClusterID
	h.acrID = attachACRID
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
