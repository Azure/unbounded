// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/registryauth"
)

// fakeMachineStatus stands in for machineStatusClient in attach-acr tests,
// returning a fixed per-node registry-auth readiness map instead of reading
// real Machines. healthy maps node name to ready.
type fakeMachineStatus struct {
	healthy map[string]bool
}

func (f fakeMachineStatus) flexMachines(_ context.Context, _ *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
	statuses := make(map[string]flexNodeStatus, len(f.healthy))
	for name, ok := range f.healthy {
		statuses[name] = flexNodeStatus{name: name, hasMachine: true, ready: ok}
	}

	return statuses, nil
}

// machineStatusReaderFunc adapts a function to machineStatusReader for tests
// that want to observe or vary the readiness polling.
type machineStatusReaderFunc func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error)

func (f machineStatusReaderFunc) flexMachines(ctx context.Context, policy *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
	return f(ctx, policy)
}

const (
	attachSubscription      = "11111111-2222-3333-4444-555555555555"
	attachACRID             = "/subscriptions/" + attachSubscription + "/resourceGroups/registry-rg/providers/Microsoft.ContainerRegistry/registries/central"
	attachClusterID         = "/subscriptions/" + attachSubscription + "/resourceGroups/cluster-rg/providers/Microsoft.ContainerService/managedClusters/cluster"
	attachIdentityID        = "/subscriptions/" + attachSubscription + "/resourceGroups/cluster-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/pull"
	attachClientID          = "22222222-2222-3333-4444-555555555555"
	attachTenantID          = "33333333-2222-3333-4444-555555555555"
	attachPrincipalID       = "44444444-2222-3333-4444-555555555555"
	attachOtherSubscription = "aaaaaaaa-2222-3333-4444-555555555555"
)

func attachFixture(t *testing.T) (*attachACRHandler, *fake.Clientset, *[]string) {
	t.Helper()

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-config", Namespace: "custom-system"},
		Data:       map[string]string{"config.yaml": "# retain comment\nupstream_registries:\n- name: ghcr.io\n  endpoint: https://ghcr.io\n", "other.yaml": "preserve"},
	}
	client := fake.NewClientset(source)
	calls := []string{}
	h := &attachACRHandler{
		acrID: attachACRID, clusterID: attachClusterID, authMode: registryauth.ExternalMode,
		namespace: source.Namespace, timeout: 5 * time.Second, client: client, restConfig: &rest.Config{Host: "https://cluster.example.com"}, out: &bytes.Buffer{},
	}
	h.runAzure = func(_ context.Context, subscriptionID string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))

		acrID, err := azureResourceID(h.acrID, "Microsoft.ContainerRegistry/registries")
		require.NoError(t, err)
		clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
		require.NoError(t, err)

		switch args[0] {
		case "acr", "role":
			require.Equal(t, acrID.SubscriptionID, subscriptionID)
		case "aks", "identity", "rest":
			require.Equal(t, clusterID.SubscriptionID, subscriptionID)
		case "account":
			require.Contains(t, []string{acrID.SubscriptionID, clusterID.SubscriptionID}, subscriptionID)
		}

		identityID := strings.Replace(attachIdentityID, attachSubscription, clusterID.SubscriptionID, 1)

		switch strings.Join(args[:min(2, len(args))], " ") {
		case "account show":
			return json.Marshal(map[string]string{"id": subscriptionID, "tenantId": attachTenantID})
		case "acr show":
			require.Equal(t, []string{"acr", "show", "--name", "central", "--resource-group", "registry-rg"}, args)

			return json.Marshal(map[string]string{"id": h.acrID, "loginServer": "central.azurecr.io", "roleAssignmentMode": "LegacyRegistryPermissions"})
		case "aks show":
			require.Equal(t, []string{"aks", "show", "--name", "cluster", "--resource-group", "cluster-rg"}, args)

			return json.Marshal(map[string]any{
				"id": h.clusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded",
				"oidcIssuerProfile": map[string]bool{"enabled": true}, "securityProfile": map[string]any{"workloadIdentity": map[string]bool{"enabled": true}},
			})
		case "aks update":
			require.Equal(t, []string{
				"aks", "update", "--name", "cluster", "--resource-group", "cluster-rg",
				"--enable-oidc-issuer", "--enable-workload-identity",
			}, args)

			return []byte("{}"), nil
		case "identity list":
			return []byte("[]"), nil
		case "identity create":
			return json.Marshal(map[string]any{
				"id": identityID, "clientId": attachClientID, "tenantId": attachTenantID, "principalId": attachPrincipalID,
				"tags": map[string]string{"unbounded-cluster-id": h.clusterID},
			})
		case "role assignment":
			if args[2] == "list" {
				return []byte("[]"), nil
			}

			return []byte("{}"), nil
		case "rest --method":
			if strings.Contains(strings.Join(args, " "), "/identityBindings?") {
				return []byte(`{"value":[]}`), nil
			}

			if args[2] == "put" {
				return []byte("{}"), nil
			}

			return json.Marshal(map[string]any{"properties": map[string]any{"managedIdentity": map[string]string{"resourceId": identityID}, "provisioningState": "Succeeded"}})
		default:
			return nil, fmt.Errorf("unexpected Azure operation: %v", args)
		}
	}

	return h, client, &calls
}

func attachTestCA(t *testing.T) string {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	data, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: data}))
}

func managedAttachFixture(t *testing.T, bundles ...corev1.ClusterTrustBundleProjection) (*attachACRHandler, *fake.Clientset, *[]string) {
	t.Helper()
	h, client, calls := attachFixture(t)
	h.authMode = registryauth.IdentityBindingMode
	ca := attachTestCA(t)
	_, err := client.CoreV1().ConfigMaps(h.namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-ca", Namespace: h.namespace}, Data: map[string]string{"ca.crt": ca},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	caProjection := corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
		LocalObjectReference: corev1.LocalObjectReference{Name: "proxy-ca"},
		Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca-cert/ca.crt"}},
	}}
	if len(bundles) > 0 {
		caProjection = corev1.VolumeProjection{ClusterTrustBundle: &bundles[0]}
	}

	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		require.Equal(t, []string{metav1.DryRunAll}, action.(k8stesting.CreateActionImpl).GetCreateOptions().DryRun)

		pod := create.GetObject().(*corev1.Pod).DeepCopy()
		require.Equal(t, "gantry-acr-discovery", pod.Spec.ServiceAccountName)
		require.Equal(t, "true", pod.Annotations["azure.workload.identity/use-identity-binding"])
		pod.Spec.Containers[0].Env = []corev1.EnvVar{
			{Name: "AZURE_KUBERNETES_SNI_NAME", Value: "proxy.example.com"},
			{Name: "AZURE_KUBERNETES_TOKEN_PROXY", Value: "https://10.0.0.1"},
			{Name: "AZURE_KUBERNETES_CA_FILE", Value: "/identity/ca-cert/ca.crt"},
			{Name: "AZURE_FEDERATED_TOKEN_FILE", Value: "/identity/token"},
		}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "identity", MountPath: "/identity"}}
		pod.Spec.Volumes = []corev1.Volume{{Name: "identity", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "api://binding"}},
				caProjection,
			},
		}}}}

		return true, pod, nil
	})

	// Model the node-reported registry-auth readiness: a single agent-backed
	// Flex node whose Machine reports the managed credential provider verified.
	// waitForACRInstallation polls this through machineStatusReader, not the
	// retired per-node installer DaemonSet.
	h.machineClient = fakeMachineStatus{healthy: map[string]bool{"off-azure": true}}

	_, err = client.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "off-azure", Labels: map[string]string{registryauth.FlexNodeLabel: registryauth.FlexNodeValue, "kubernetes.io/os": "linux"}},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.0"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	// The operator's audience reconciler publishes the identity-use grant once
	// the policy is in place; the attach-acr CLI no longer creates or waits on a
	// per-node installer DaemonSet, so the fixture publishes the grant directly,
	// stamped with the policy hash the attach path will compute, so the
	// managed-attach acknowledgment (ownership + hash) succeeds.
	_, err = client.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        registryauth.IdentityRoleName,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": acrAttachManager, "unbounded-cloud.io/namespace": h.namespace},
			Annotations: map[string]string{registryAuthConfigHashKey: attachTestGrantHash(t, ca)},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: registryauth.IdentityRoleName},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	client.ClearActions()

	return h, client, calls
}

// attachTestGrantHash computes the identity-use grant hash the way the attach
// path does, from the test's fixed policy values, so the fixture can stamp the
// grant the way the operator would. The values match the fixture's ACR
// (central.azurecr.io), identity (attachClientID/attachTenantID), and the
// discovery pod's proxy env as execute() parses it: ProxyAddress is the proxy
// hostname (10.0.0.1, not the URL), and CACert is the generated CA the fixture
// injects through the proxy-ca ConfigMap.
func attachTestGrantHash(t *testing.T, ca string) string {
	t.Helper()

	cfg := registryauth.Config{
		Mode: registryauth.IdentityBindingMode, Registry: "central.azurecr.io",
		ClientID: attachClientID, TenantID: attachTenantID,
		SNIName: "proxy.example.com", ProxyAddress: "10.0.0.1", Audience: "api://binding", CACert: ca,
	}

	payload, err := json.Marshal(cfg)
	require.NoError(t, err)

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

func TestGantryAttachACRWaitsForAdmissionReadiness(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "metadata not injected yet"},
		{name: "webhook temporarily unavailable", err: apierrors.NewServiceUnavailable("webhook starting")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, _ := managedAttachFixture(t)
			h.timeout = 15 * time.Second
			attempts := 0

			client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				if attempts == 1 {
					return true, action.(k8stesting.CreateAction).GetObject(), tc.err
				}

				return false, nil, nil
			})

			require.NoError(t, h.execute(t.Context()))
			require.Equal(t, 2, attempts)
		})
	}
}

func TestGantryAttachACRAdmissionWaitRejectsPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy string
		err   error
	}{
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "discover", errors.New("denied"))},
		{name: "invalid proxy", proxy: "http://untrusted.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, _ := managedAttachFixture(t)
			attempts := 0

			client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "AZURE_KUBERNETES_TOKEN_PROXY", Value: tc.proxy}}

				return true, pod, tc.err
			})

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, ClientID: attachClientID, TenantID: attachTenantID}
			err := h.waitForACRProxy(ctx, &cfg)
			require.Error(t, err)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 1, attempts)
		})
	}
}

func TestGantryAttachACRAdmissionWaitHonorsCancellation(t *testing.T) {
	h, client, _ := managedAttachFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	attempts := 0

	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts++

		cancel()

		return true, action.(k8stesting.CreateAction).GetObject(), nil
	})

	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, SNIName: "original.example.com"}
	original := cfg
	require.ErrorIs(t, h.waitForACRProxy(ctx, &cfg), context.Canceled)
	require.Equal(t, 1, attempts)
	require.Equal(t, original, cfg)
}

func TestGantryAttachACRExternalPreservesCustomerAuthentication(t *testing.T) {
	for _, tc := range []struct{ name, mode, policy string }{
		{name: "default without policy"},
		{name: "external without policy", mode: registryauth.ExternalMode},
		{name: "default with customer policy", policy: "customer-owned-policy: untouched"},
		{name: "external with customer policy", mode: registryauth.ExternalMode, policy: "customer-owned-policy: untouched"},
		{name: "default with managed policy", policy: "mode: aks-identity-binding\nregistry: central.azurecr.io\n"},
		{name: "external with managed policy", mode: registryauth.ExternalMode, policy: "mode: aks-identity-binding\nregistry: central.azurecr.io\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, calls := attachFixture(t)
			h.authMode = tc.mode
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			if tc.policy != "" {
				source.Data[registryauth.ConfigKey] = tc.policy
			}

			source.Data["config.yaml"] += "  credentials_path: /shared/auth\n"
			_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
			require.NoError(t, err)
			client.ClearActions()
			require.NoError(t, h.execute(t.Context()))
			require.Len(t, *calls, 1)
			require.Contains(t, (*calls)[0], "acr show")
			got, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, tc.policy, got.Data[registryauth.ConfigKey])

			if tc.policy == "" {
				require.NotContains(t, got.Data, registryauth.ConfigKey)
			}

			require.Equal(t, "preserve", got.Data["other.yaml"])
			require.Contains(t, got.Data["config.yaml"], "# retain comment")
			require.Contains(t, got.Data["config.yaml"], "credentials_path: /shared/auth")
			require.Contains(t, got.Data["config.yaml"], "central.azurecr.io")
			require.NotContains(t, h.out.(*bytes.Buffer).String(), "WARNING:")

			for _, action := range client.Actions() {
				require.Equal(t, "configmaps", action.GetResource().Resource)
			}

			require.NoError(t, h.execute(t.Context()))
		})
	}
}

func TestGantryAttachACRRejectsUpstreamAliasCollisionsBeforeWrites(t *testing.T) {
	for _, mode := range []string{registryauth.ExternalMode, registryauth.IdentityBindingMode} {
		t.Run(mode, func(t *testing.T) {
			h, client, calls := attachFixture(t)
			h.authMode = mode
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			source.Data["config.yaml"] += "  ns_alias: central.azurecr.io\n"
			_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
			require.NoError(t, err)
			client.ClearActions()

			require.ErrorContains(t, h.execute(t.Context()), "ns_alias")
			require.Len(t, *calls, 1)

			for _, action := range client.Actions() {
				require.Equal(t, "get", action.GetVerb(), "a collision must fail before Kubernetes writes")
			}

			current, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, source.Data, current.Data)
		})
	}
}

func TestGantryAttachACRABACRoleSelection(t *testing.T) {
	const readerRole = "55555555-2222-3333-4444-555555555555"

	for _, tc := range []struct {
		name              string
		roles             []map[string]string
		crossSubscription bool
		wantErr           bool
	}{
		{name: "repository reader", roles: []map[string]string{{"name": readerRole, "roleType": "BuiltInRole"}}},
		{name: "cross-subscription repository reader", roles: []map[string]string{{"name": readerRole, "roleType": "BuiltInRole"}}, crossSubscription: true},
		{name: "unavailable", wantErr: true},
		{name: "ambiguous", roles: []map[string]string{{"name": readerRole, "roleType": "BuiltInRole"}, {"name": attachClientID, "roleType": "BuiltInRole"}}, wantErr: true},
		{name: "custom role", roles: []map[string]string{{"name": readerRole, "roleType": "CustomRole"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			if tc.crossSubscription {
				h.acrID = strings.Replace(attachACRID, attachSubscription, attachOtherSubscription, 1)
			}

			run := h.runAzure
			h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
				command := strings.Join(args, " ")
				if strings.HasPrefix(command, "role definition list") {
					acrID, err := azureResourceID(h.acrID, "Microsoft.ContainerRegistry/registries")
					require.NoError(t, err)
					require.Equal(t, acrID.SubscriptionID, subscriptionID)

					*calls = append(*calls, command)

					return json.Marshal(tc.roles)
				}

				data, err := run(ctx, subscriptionID, args...)
				if err != nil || !strings.HasPrefix(command, "acr show") {
					return data, err
				}

				var registry map[string]string
				require.NoError(t, json.Unmarshal(data, &registry))
				registry["roleAssignmentMode"] = "AbacRepositoryPermissions"

				return json.Marshal(registry)
			}

			err := h.execute(t.Context())
			source, readErr := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, readErr)

			if tc.wantErr {
				require.ErrorContains(t, err, "repository-reader role is unavailable or ambiguous")
				require.Empty(t, source.Data[registryauth.ConfigKey])

				for _, call := range *calls {
					require.NotContains(t, call, "role assignment create")
				}
			} else {
				require.NoError(t, err)
				_, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
				require.NoError(t, err)
				// The created grant is stamped with the deterministic
				// ownership-receipt name (the role-assignment API has no tags
				// property) so detach can reverse exactly the grant this
				// installation created, and leave a shared/reused grant in
				// place.
				require.Contains(t, *calls, "role assignment create --assignee-object-id "+attachPrincipalID+
					" --assignee-principal-type ServicePrincipal --role "+readerRole+" --scope "+h.acrID+
					" --name "+acrPullGrantName(h.clusterID, h.acrID, attachPrincipalID, readerRole))
			}
		})
	}
}

// TestGantryAttachACRTwoScopesUseDistinctGrantNames verifies that attaching the
// same cluster to two different ACRs produces two role-assignment names that do
// not collide: Azure role-assignment names are tenant-wide unique, so the
// deterministic receipt must include the ACR scope. The sequence is attach
// registry A, detach without Azure cleanup (the default), then attach registry
// B. The surviving grant on A and the new grant on B must have different names.
func TestGantryAttachACRTwoScopesUseDistinctGrantNames(t *testing.T) {
	// Two ACRs in different resource groups (and potentially subscriptions).
	acrA := "/subscriptions/11111111-2222-3333-4444-555555555555/resourceGroups/registry-rg/providers/Microsoft.ContainerRegistry/registries/central"
	acrB := "/subscriptions/11111111-2222-3333-4444-555555555555/resourceGroups/other-rg/providers/Microsoft.ContainerRegistry/registries/other"

	const acrPullRole = "7f951dda-4ed3-4680-a7ca-43fe172d538d"

	nameA := acrPullGrantName(attachClusterID, acrA, attachPrincipalID, acrPullRole)
	nameB := acrPullGrantName(attachClusterID, acrB, attachPrincipalID, acrPullRole)

	// The names must differ: a collision would cause the second create to fail
	// with RoleAssignmentUpdateNotPermitted.
	require.NotEqual(t, nameA, nameB, "two ACRs must produce distinct grant names")

	// The names must be deterministic: calling again yields the same name.
	require.Equal(t, nameA, acrPullGrantName(attachClusterID, acrA, attachPrincipalID, acrPullRole))
	require.Equal(t, nameB, acrPullGrantName(attachClusterID, acrB, attachPrincipalID, acrPullRole))
}

func TestGantryAttachACRClusterTrustBundleSelection(t *testing.T) {
	selected, missing, signer := "selected", "missing", "example.com/identity-binding"

	unrelatedCA := attachTestCA(t)
	for _, tc := range []struct {
		name       string
		projection corev1.ClusterTrustBundleProjection
		wantErr    bool
	}{
		{name: "named bundle", projection: corev1.ClusterTrustBundleProjection{Name: &selected}},
		{name: "signer and labels", projection: corev1.ClusterTrustBundleProjection{
			SignerName: &signer, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"purpose": "acr"}},
		}},
		{name: "empty match", projection: corev1.ClusterTrustBundleProjection{Name: &missing}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.projection.Path = "ca-cert/ca.crt"
			h, client, calls := managedAttachFixture(t, tc.projection)
			fixtureCA, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "proxy-ca", metav1.GetOptions{})
			require.NoError(t, err)

			ca := fixtureCA.Data["ca.crt"]
			for _, bundle := range []certificatesv1beta1.ClusterTrustBundle{
				{
					ObjectMeta: metav1.ObjectMeta{Name: selected, Labels: map[string]string{"purpose": "acr"}},
					Spec:       certificatesv1beta1.ClusterTrustBundleSpec{SignerName: signer, TrustBundle: ca},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "other-signer", Labels: map[string]string{"purpose": "acr"}},
					Spec:       certificatesv1beta1.ClusterTrustBundleSpec{SignerName: "example.com/other", TrustBundle: unrelatedCA},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "other-label", Labels: map[string]string{"purpose": "other"}},
					Spec:       certificatesv1beta1.ClusterTrustBundleSpec{SignerName: signer, TrustBundle: unrelatedCA},
				},
			} {
				_, err := client.CertificatesV1beta1().ClusterTrustBundles().Create(t.Context(), &bundle, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)
			err = h.execute(t.Context())
			current, readErr := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
			require.NoError(t, readErr)

			if tc.wantErr {
				require.ErrorContains(t, err, "incomplete AKS identity-binding admission metadata")
				require.Equal(t, source.Data, current.Data)

				for _, call := range *calls {
					require.NotContains(t, call, "role assignment create")
				}
			} else {
				require.NoError(t, err)
				cfg, err := registryauth.Derive(current.Data["config.yaml"], current.Data[registryauth.ConfigKey])
				require.NoError(t, err)
				require.Equal(t, ca, cfg.CACert)
			}
		})
	}
}

func TestGantryManagedACRWarningPrecedesEverySideEffect(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
		require.Contains(t, h.out.(*bytes.Buffer).String(), "WARNING:")

		return run(ctx, subscriptionID, args...)
	}

	client.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		require.Contains(t, h.out.(*bytes.Buffer).String(), "WARNING:")
		return false, nil, nil
	})
	require.NoError(t, h.execute(t.Context()))
}

func TestGantryAttachACRRejectsInvalidAuthModeBeforeWrites(t *testing.T) {
	h, client, calls := attachFixture(t)
	h.authMode = "unsupported"
	require.ErrorContains(t, h.execute(t.Context()), "--auth-mode must be")
	require.Empty(t, *calls)
	require.Empty(t, client.Actions())

	h.authMode, h.clusterID = registryauth.IdentityBindingMode, ""
	require.ErrorContains(t, h.execute(t.Context()), "--cluster-resource-id")
	require.Empty(t, *calls)
	require.Empty(t, client.Actions())
}

func TestGantryAttachACRMergesConcurrentConfiguration(t *testing.T) {
	h, client, _ := attachFixture(t)
	conflicted := false

	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}

		conflicted = true
		cm := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		cm.Data["other.yaml"] = "concurrent edit"
		require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, h.namespace))

		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("concurrent edit"))
	})
	require.NoError(t, h.execute(t.Context()))
	got, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "concurrent edit", got.Data["other.yaml"])
}

// TestGantryAttachACRReattachUpdatesOwnedDiscoveryServiceAccount verifies the
// reattach-with-a-fresh-identity path: when a prior attach created the owned
// gantry-acr-discovery ServiceAccount annotated with an old (stale) client-id,
// and a re-attach mints a new identity, the attach updates the owned account's
// identity annotations in place rather than refusing. A truly foreign account
// (wrong managed-by label) is still refused.
func TestGantryAttachACRReattachUpdatesOwnedDiscoveryServiceAccount(t *testing.T) {
	h, client, _ := managedAttachFixture(t)

	staleClientID := "11111111-0000-0000-0000-000000000000"
	staleTenantID := "44444444-0000-0000-0000-000000000000"

	// An owned discovery ServiceAccount from a prior attach, carrying a stale
	// (old) identity. The re-attach must update it to the fresh identity the
	// Azure fixture returns (attachClientID/attachTenantID).
	_, err := client.CoreV1().ServiceAccounts(h.namespace).Create(t.Context(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gantry-acr-discovery", Namespace: h.namespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": acrAttachManager},
			Annotations: map[string]string{"azure.workload.identity/client-id": staleClientID, "azure.workload.identity/tenant-id": staleTenantID},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, h.execute(t.Context()))

	got, err := client.CoreV1().ServiceAccounts(h.namespace).Get(t.Context(), "gantry-acr-discovery", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, attachClientID, got.Annotations["azure.workload.identity/client-id"],
		"the owned discovery account must be updated to the fresh client-id, not refused")
	require.Equal(t, attachTenantID, got.Annotations["azure.workload.identity/tenant-id"],
		"the owned discovery account must be updated to the fresh tenant-id")
}

// TestGantryAttachACRReattachRefusesForeignDiscoveryServiceAccount verifies a
// foreign (unowned) discovery ServiceAccount is still refused, even when the
// attach would update an owned one: the managed-by label must match.
func TestGantryAttachACRReattachRefusesForeignDiscoveryServiceAccount(t *testing.T) {
	h, client, _ := managedAttachFixture(t)

	// A foreign discovery ServiceAccount: wrong managed-by label.
	_, err := client.CoreV1().ServiceAccounts(h.namespace).Create(t.Context(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gantry-acr-discovery", Namespace: h.namespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "customer"},
			Annotations: map[string]string{"azure.workload.identity/client-id": "99999999-9999-9999-9999-999999999999"},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.ErrorContains(t, h.execute(t.Context()), "unowned")

	// The foreign account is untouched.
	got, err := client.CoreV1().ServiceAccounts(h.namespace).Get(t.Context(), "gantry-acr-discovery", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "customer", got.Labels["app.kubernetes.io/managed-by"], "a foreign account must not be changed")
}

func TestGantryAttachACRManagedUsesStockProviderWithoutComputeAssignment(t *testing.T) {
	h, client, calls := managedAttachFixture(t)
	require.NoError(t, h.execute(t.Context()))
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	cfg, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
	require.NoError(t, err)
	require.Equal(t, attachClientID, cfg.ClientID)
	require.Equal(t, "central.azurecr.io", cfg.Registry)
	binding, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, binding.Subjects, 1)
	require.Equal(t, "system:serviceaccounts", binding.Subjects[0].Name)

	for _, call := range *calls {
		require.NotContains(t, call, "vmss")
		require.NotContains(t, call, "vm identity")
	}

	for _, action := range client.Actions() {
		if action.GetResource().Resource == "serviceaccounts" && action.GetVerb() == "create" {
			require.Equal(t, "gantry-acr-discovery", action.(k8stesting.CreateAction).GetObject().(*corev1.ServiceAccount).Name)
		}
	}
}

func TestGantryAttachACRManagedCrossSubscription(t *testing.T) {
	denied := errors.New("role assignment permission denied")

	for _, tc := range []struct {
		name          string
		existingGrant bool
		grantErr      error
	}{
		{name: "create pull grant"},
		{name: "reuse pull grant", existingGrant: true},
		{name: "missing role-assignment permission", grantErr: denied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			h.acrID = strings.Replace(attachACRID, attachSubscription, attachOtherSubscription, 1)
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			var accounts []string

			run := h.runAzure
			h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
				if args[0] == "account" {
					accounts = append(accounts, subscriptionID)
				}

				if args[0] == "role" && args[1] == "assignment" {
					require.Equal(t, attachOtherSubscription, subscriptionID)

					if args[2] == "list" && tc.existingGrant {
						*calls = append(*calls, strings.Join(args, " "))

						return json.Marshal([]map[string]string{{
							"principalId": attachPrincipalID, "scope": h.acrID,
							"roleDefinitionId": "/subscriptions/" + attachOtherSubscription +
								"/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d",
						}})
					}

					if args[2] == "create" && tc.grantErr != nil {
						return nil, tc.grantErr
					}
				}

				return run(ctx, subscriptionID, args...)
			}

			err = h.execute(t.Context())
			require.ElementsMatch(t, []string{attachSubscription, attachOtherSubscription}, accounts)
			current, readErr := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
			require.NoError(t, readErr)

			if tc.grantErr != nil {
				require.ErrorIs(t, err, tc.grantErr)
				require.Equal(t, source.Data, current.Data)

				return
			}

			require.NoError(t, err)
			cfg, err := registryauth.Derive(current.Data["config.yaml"], current.Data[registryauth.ConfigKey])
			require.NoError(t, err)
			require.Equal(t, attachClientID, cfg.ClientID)
			require.Equal(t, attachTenantID, cfg.TenantID)

			if tc.existingGrant {
				for _, call := range *calls {
					require.NotContains(t, call, "role assignment create")
				}
			} else {
				// The created grant is stamped with the deterministic
				// ownership-receipt name (the role-assignment API has no tags
				// property) so detach can reverse exactly the grant this
				// installation created, and leave a shared/reused grant in
				// place.
				require.Contains(t, *calls, "role assignment create --assignee-object-id "+attachPrincipalID+
					" --assignee-principal-type ServicePrincipal --role 7f951dda-4ed3-4680-a7ca-43fe172d538d --scope "+h.acrID+
					" --name "+acrPullGrantName(h.clusterID, h.acrID, attachPrincipalID, "7f951dda-4ed3-4680-a7ca-43fe172d538d"))
			}
		})
	}
}

func TestGantryAttachACRRejectsIdentityOutsideClusterSubscription(t *testing.T) {
	for _, tc := range []struct{ name, field, value, wantErr string }{
		{
			name: "identity in registry subscription", field: "id",
			value: strings.Replace(attachIdentityID, attachSubscription, attachOtherSubscription, 1), wantErr: "AKS cluster subscription",
		},
		{name: "identity in another tenant", field: "tenantId", value: attachPrincipalID, wantErr: "identity tenant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			h.acrID = strings.Replace(attachACRID, attachSubscription, attachOtherSubscription, 1)
			run := h.runAzure
			h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
				data, err := run(ctx, subscriptionID, args...)
				if err != nil || args[0] != "identity" || args[1] != "create" {
					return data, err
				}

				var identity map[string]any
				require.NoError(t, json.Unmarshal(data, &identity))
				identity[tc.field] = tc.value

				return json.Marshal(identity)
			}

			require.ErrorContains(t, h.execute(t.Context()), tc.wantErr)
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, source.Data[registryauth.ConfigKey])

			for _, call := range *calls {
				require.NotContains(t, call, "rest --method put")
				require.NotContains(t, call, "role assignment create")
			}
		})
	}
}

// TestGantryAttachACRWaitsOnNodeReportedState verifies the CLI waits on the
// node-reported Machine.Status registry-auth readiness (via machineClient), not
// on the retired per-node installer DaemonSet, and never polls the DaemonSet.
func TestGantryAttachACRWaitsOnNodeReportedState(t *testing.T) {
	h, client, _ := managedAttachFixture(t)

	polls := 0
	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		polls++

		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: true}}, nil
	})

	require.NoError(t, h.execute(t.Context()))
	require.GreaterOrEqual(t, polls, 1, "attach must confirm the node-reported registry-auth state")

	for _, action := range client.Actions() {
		require.False(t, action.Matches("get", "daemonsets"), "attach-acr must not poll the retired installer DaemonSet")
	}
}

func TestGantryAttachACRWaitsForHealthyNodeReport(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	h.timeout = 3 * time.Second

	polls := 0
	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		polls++
		// The node is not yet verified: keep it not-ready so the wait retries
		// until the timeout rather than acknowledging a stale state.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, detail: "still converging"}}, nil
	})

	// An unhealthy node report must not be acknowledged; the wait must run to
	// the timeout.
	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
	require.GreaterOrEqual(t, polls, 2, "an unhealthy node report must be retried, not acknowledged")

	_ = client
}

func TestGantryAttachACRDoesNotReachInstallationWhenPolicySaveFails(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("policy write failed")
	})

	polls := 0
	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		polls++

		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: true}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "policy write failed")
	require.Zero(t, polls, "a failed opt-in must not proceed to wait for node-reported installation")

	_ = client
}

// TestFlexMachineStatusCountercases verifies the structured, per-node status
// the CLI waits on is mode-aware: a True condition is not enough. A managed
// attach must not count a node as ready when the node reports the external
// policy (reason mismatch), a stale condition (observed generation behind the
// machine generation), or a delivered spec that does not match the desired
// policy. This is the fix for the review finding that a mode-blind bool let a
// managed attach succeed on External, a stale Configured, or a missing Machine.
func TestFlexMachineStatusCountercases(t *testing.T) {
	t.Parallel()

	managed := &v1alpha3.RegistryAuthPolicy{
		Mode:     registryauth.IdentityBindingMode,
		Registry: "example.azurecr.io",
		ClientID: "11111111-2222-3333-4444-555555555555",
		TenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}

	// A node reporting the external reason is not ready for a managed attach.
	externalCond := &metav1.Condition{
		Type: v1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue,
		Reason: "External", ObservedGeneration: 1,
	}

	externalMachine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: 1},
		Spec:       v1alpha3.MachineSpec{RegistryAuth: managed},
		Status:     v1alpha3.MachineStatus{Conditions: []metav1.Condition{*externalCond}},
	}

	st := flexMachineStatus(externalMachine, managed, "Configured")
	require.False(t, st.ready, "an External condition must not count as a managed attach")
	require.Contains(t, st.detail, "External")

	// A stale Configured (observed generation behind the machine generation) is
	// not ready.
	staleCond := &metav1.Condition{
		Type: v1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue,
		Reason: "Configured", ObservedGeneration: 0,
	}
	staleMachine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: 1},
		Spec:       v1alpha3.MachineSpec{RegistryAuth: managed},
		Status:     v1alpha3.MachineStatus{Conditions: []metav1.Condition{*staleCond}},
	}

	st = flexMachineStatus(staleMachine, managed, "Configured")
	require.False(t, st.ready, "a stale condition must not count as ready")
	require.Contains(t, st.detail, "lags machine generation")

	// A delivered spec that does not match the desired policy is not ready, even
	// with a current-generation Configured condition.
	otherPolicy := &v1alpha3.RegistryAuthPolicy{
		Mode:     registryauth.IdentityBindingMode,
		Registry: "other.azurecr.io",
		ClientID: "11111111-2222-3333-4444-555555555555",
		TenantID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	mismatchCond := &metav1.Condition{
		Type: v1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue,
		Reason: "Configured", ObservedGeneration: 1,
	}
	mismatchMachine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: 1},
		Spec:       v1alpha3.MachineSpec{RegistryAuth: otherPolicy},
		Status:     v1alpha3.MachineStatus{Conditions: []metav1.Condition{*mismatchCond}},
	}

	st = flexMachineStatus(mismatchMachine, managed, "Configured")
	require.False(t, st.ready, "a delivered spec that does not match the desired policy must not count as ready")

	// The matching case: current-generation Configured and a matching spec is
	// ready.
	matchCond := &metav1.Condition{
		Type: v1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionTrue,
		Reason: "Configured", ObservedGeneration: 1,
	}
	matchMachine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: 1},
		Spec:       v1alpha3.MachineSpec{RegistryAuth: managed},
		Status:     v1alpha3.MachineStatus{Conditions: []metav1.Condition{*matchCond}},
	}

	st = flexMachineStatus(matchMachine, managed, "Configured")
	require.True(t, st.ready, "a current-generation Configured with a matching spec must be ready")
	require.True(t, st.hasMachine)
}

// TestGantryAttachACRManagedDoesNotSucceedOnExternalReport verifies the managed
// attach wait does not complete when a node reports the external policy (reason
// mismatch), even if the condition is True and at the current generation: the
// structured status rejects it, so the wait retries until the timeout.
func TestGantryAttachACRManagedDoesNotSucceedOnExternalReport(t *testing.T) {
	h, _, _ := managedAttachFixture(t)
	h.timeout = 2 * time.Second

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		// The node reports the external reason (not Configured): a mode-blind
		// reader would have counted this as healthy.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, detail: "condition reports External, expected Configured for this policy"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
}

// TestGantryAttachACRDoesNotSucceedOnMissingMachine verifies the managed attach
// wait does not complete when an eligible node has no agent-backed Machine yet:
// a hasMachine=false, ready=false status is an eligible unverified node, not a
// converged one, so it is retried until the timeout (the reader deliberately
// retains it, and the wait must not skip it).
func TestGantryAttachACRDoesNotSucceedOnMissingMachine(t *testing.T) {
	h, _, _ := managedAttachFixture(t)
	h.timeout = 2 * time.Second

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		// A node with no Machine yet: the reader retains it as eligible but
		// unverified. A wait that only checked hasMachine && !ready would skip
		// it and declare success immediately.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: false, ready: false, detail: "no agent-backed Machine registered yet"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "timed out waiting for Flex nodes")
}

// TestGantryAttachACRTerminalFailureSurfacesImmediately verifies a node that
// reports a terminal (False) condition is surfaced immediately rather than
// polled to the timeout: the wait must stop at the first terminal report.
func TestGantryAttachACRTerminalFailureSurfacesImmediately(t *testing.T) {
	h, _, _ := managedAttachFixture(t)
	h.timeout = 30 * time.Second

	start := time.Now()

	h.machineClient = machineStatusReaderFunc(func(context.Context, *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
		// A terminal False condition: the node reports the policy will not
		// converge.
		return map[string]flexNodeStatus{"off-azure": {name: "off-azure", hasMachine: true, ready: false, terminal: true, detail: "condition False: RestartBudgetExhausted (budget spent)"}}, nil
	})

	require.ErrorContains(t, h.execute(t.Context()), "terminal failure")
	// A terminal failure is surfaced immediately, well before the timeout.
	require.WithinDuration(t, time.Now(), start, 3*time.Second, "a terminal failure must not be polled to the timeout")
}

// TestFlexMachineStatusTerminal distinguishes a genuinely terminal failure from
// a retryable one. Only a current-generation RestartBudgetExhausted for the
// DELIVERED policy is terminal: the agent reports other False reasons while it
// is scheduling a bounded retry, a stale failure from a prior generation is not
// terminal, and a failure for a different (undelivered) requested policy is not
// terminal because the operator has not yet patched the new policy onto the spec.
func TestFlexMachineStatusTerminal(t *testing.T) {
	// A managed policy the machine may or may not carry on its spec.
	managedPolicy := &v1alpha3.RegistryAuthPolicy{Mode: "aks-identity-binding", Registry: "central.azurecr.io"}

	tt := []struct {
		name         string
		reason       string
		observedGen  int64
		machineGen   int64
		spec         *v1alpha3.RegistryAuthPolicy // the delivered spec on the machine
		requested    *v1alpha3.RegistryAuthPolicy // the requested policy (nil = external/detach)
		wantTerminal bool
	}{
		// Current-gen exhaustion for the delivered policy: terminal.
		{name: "current-gen budget exhaustion for delivered policy is terminal", reason: "RestartBudgetExhausted", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: managedPolicy, wantTerminal: true},
		// Retryable reasons are never terminal.
		{name: "retryable backoff is not terminal", reason: "RestartBackoff", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: managedPolicy, wantTerminal: false},
		{name: "retryable apply failure is not terminal", reason: "ApplyFailed", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: managedPolicy, wantTerminal: false},
		// Stale-generation exhaustion is not terminal.
		{name: "stale-generation budget exhaustion is not terminal", reason: "RestartBudgetExhausted", observedGen: 1, machineGen: 2, spec: managedPolicy, requested: managedPolicy, wantTerminal: false},
		// Exhaustion for a DIFFERENT requested policy (spec does not match): not terminal.
		{name: "budget exhaustion for undelivered requested policy is not terminal", reason: "RestartBudgetExhausted", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: nil, wantTerminal: false},
		// Detach (nil requested) while the machine carries an old exhausted managed
		// policy: not terminal because the spec does not match the requested (nil).
		{name: "detach with old exhausted managed spec is not terminal", reason: "RestartBudgetExhausted", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: nil, wantTerminal: false},
		// New managed policy requested while the machine carries an old exhausted
		// managed policy (different registry): not terminal.
		{name: "new managed policy with old exhausted spec is not terminal", reason: "RestartBudgetExhausted", observedGen: 1, machineGen: 1, spec: managedPolicy, requested: &v1alpha3.RegistryAuthPolicy{Mode: "aks-identity-binding", Registry: "other.azurecr.io"}, wantTerminal: false},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			cond := &metav1.Condition{
				Type: v1alpha3.MachineConditionRegistryAuth, Status: metav1.ConditionFalse,
				Reason: tc.reason, ObservedGeneration: tc.observedGen,
			}
			machine := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: tc.machineGen},
				Spec:       v1alpha3.MachineSpec{RegistryAuth: tc.spec},
				Status:     v1alpha3.MachineStatus{Conditions: []metav1.Condition{*cond}},
			}

			wantReason := "External"
			if tc.requested != nil {
				wantReason = "Configured"
			}

			st := flexMachineStatus(machine, tc.requested, wantReason)
			require.Equal(t, tc.wantTerminal, st.terminal, "terminal flag")
			require.False(t, st.ready, "a False condition is never ready")
		})
	}

	// A not-ready-but-converging status (no condition) is not terminal.
	empty := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "n", Generation: 1}}
	st := flexMachineStatus(empty, nil, "External")
	require.False(t, st.terminal, "a not-yet-reported node is not terminal")
	require.False(t, st.ready)
}

func TestGantryAttachACRRejectsConcurrentInvalidEndpoint(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
		if args[0] == "identity" && args[1] == "create" {
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			source.Data["config.yaml"] += "- name: central.azurecr.io\n  endpoint: http://central.azurecr.io\n"
			_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
			require.NoError(t, err)
		}

		return run(ctx, subscriptionID, args...)
	}
	require.ErrorContains(t, h.execute(t.Context()), "matching HTTPS endpoint")
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, source.Data, registryauth.ConfigKey, "invalid merged policy must not be persisted")
}

func TestGantryAttachACRRejectsWrongClusterBeforeAzureWrites(t *testing.T) {
	h, _, calls := managedAttachFixture(t)
	h.restConfig.Host = "https://different.example.com"
	require.ErrorContains(t, h.execute(t.Context()), "does not match")
	require.Len(t, *calls, 2)
}

func TestGantryAttachACREnablesPrerequisitesWithNamedCluster(t *testing.T) {
	for _, subscriptionID := range []string{attachSubscription, attachOtherSubscription} {
		t.Run(subscriptionID, func(t *testing.T) {
			h, _, calls := managedAttachFixture(t)
			h.acrID = strings.Replace(attachACRID, attachSubscription, subscriptionID, 1)
			run := h.runAzure
			h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
				data, err := run(ctx, subscriptionID, args...)
				if err != nil || !strings.HasPrefix(strings.Join(args, " "), "aks show ") {
					return data, err
				}

				var cluster map[string]any
				if err := json.Unmarshal(data, &cluster); err != nil {
					return nil, err
				}

				cluster["oidcIssuerProfile"] = map[string]bool{"enabled": false}

				return json.Marshal(cluster)
			}
			require.NoError(t, h.execute(t.Context()))
			require.Contains(t, *calls, "aks update --name cluster --resource-group cluster-rg --enable-oidc-issuer --enable-workload-identity")
		})
	}
}

func TestGantryAttachACRManagedRerunsReuseOwnedResources(t *testing.T) {
	h, _, calls := managedAttachFixture(t)
	require.NoError(t, h.execute(t.Context()))

	digest := sha256.Sum256([]byte(strings.ToLower(attachClusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])
	run := h.runAzure
	h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
		command := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(command, "identity list"):
			return json.Marshal([]map[string]any{{
				"name": name, "id": attachIdentityID, "clientId": attachClientID,
				"tenantId": attachTenantID, "principalId": attachPrincipalID, "tags": map[string]string{"unbounded-cluster-id": attachClusterID},
			}})
		case strings.Contains(command, "/identityBindings?"):
			return json.Marshal(map[string]any{"value": []map[string]any{{
				"name":       name,
				"properties": map[string]any{"managedIdentity": map[string]string{"resourceId": attachIdentityID}},
			}}})
		case strings.HasPrefix(command, "role assignment list"):
			return json.Marshal([]map[string]string{{
				"principalId": attachPrincipalID, "scope": attachACRID,
				"roleDefinitionId": "/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d",
			}})
		default:
			return run(ctx, subscriptionID, args...)
		}
	}
	require.NoError(t, h.execute(t.Context()))

	for _, operation := range []string{"identity create", "rest --method put", "role assignment create"} {
		var count int

		for _, call := range *calls {
			if strings.HasPrefix(call, operation) {
				count++
			}
		}

		require.Equal(t, 1, count, operation)
	}
}

func TestGantryAttachACRManagedRestoresRemovedUpstream(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	require.NoError(t, h.execute(t.Context()))
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)

	policy := source.Data[registryauth.ConfigKey]
	source.Data["config.yaml"] = "# administrator edit\nupstream_registries: []\n"
	_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.execute(t.Context()))

	source, err = client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, policy, source.Data[registryauth.ConfigKey])
	require.Contains(t, source.Data["config.yaml"], "# administrator edit")
	cfg, err := registryauth.Derive(source.Data["config.yaml"], policy)
	require.NoError(t, err)
	require.Equal(t, "central.azurecr.io", cfg.Registry)
}

func TestGantryAttachACRRejectsOldKubeletsAndCrossSubscription(t *testing.T) {
	t.Run("old kubelet", func(t *testing.T) {
		h, client, calls := managedAttachFixture(t)
		node, err := client.CoreV1().Nodes().Get(t.Context(), "off-azure", metav1.GetOptions{})
		require.NoError(t, err)

		node.Status.NodeInfo.KubeletVersion = "v1.33.9"
		_, err = client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.ErrorContains(t, h.execute(t.Context()), "1.34")
		require.Len(t, *calls, 2)
	})

	for _, tc := range []struct{ name, subscriptionID, tenantID, wantErr string }{
		{name: "cross-subscription tenant mismatch", subscriptionID: attachOtherSubscription, tenantID: attachPrincipalID, wantErr: "same tenant"},
		{name: "missing subscription tenant", subscriptionID: attachOtherSubscription, wantErr: "invalid tenant ID"},
		{name: "invalid subscription tenant", subscriptionID: attachOtherSubscription, tenantID: "not-a-tenant", wantErr: "invalid tenant ID"},
		{name: "wrong subscription response", subscriptionID: attachSubscription, tenantID: attachTenantID, wantErr: "requested subscription"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			h.acrID = strings.Replace(attachACRID, attachSubscription, attachOtherSubscription, 1)
			run := h.runAzure
			h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
				if args[0] == "account" && subscriptionID == attachOtherSubscription {
					return json.Marshal(map[string]string{"id": tc.subscriptionID, "tenantId": tc.tenantID})
				}

				return run(ctx, subscriptionID, args...)
			}

			require.ErrorContains(t, h.execute(t.Context()), tc.wantErr)

			for _, call := range *calls {
				require.NotContains(t, call, "aks update")
				require.NotContains(t, call, "identity create")
				require.NotContains(t, call, "rest --method put")
				require.NotContains(t, call, "role assignment create")
			}

			for _, action := range client.Actions() {
				require.Contains(t, []string{"get", "list"}, action.GetVerb(), "tenant validation must fail before Kubernetes writes")
			}
		})
	}
}

func TestGantryAttachACRRejectsMissingAdmissionAndConditionalGrants(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		t.Run(fmt.Sprint(conditional), func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			if conditional {
				run := h.runAzure
				h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
					if strings.HasPrefix(strings.Join(args, " "), "role assignment list") {
						require.Contains(t, args, "--include-inherited")

						return json.Marshal([]map[string]string{{
							"principalId": attachPrincipalID, "scope": "/subscriptions/" + attachSubscription,
							"roleDefinitionId": "/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d", "condition": "repository restriction",
						}})
					}

					return run(ctx, subscriptionID, args...)
				}
				require.ErrorContains(t, h.execute(t.Context()), "conditional")
			} else {
				client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, action.(k8stesting.CreateAction).GetObject(), nil
				})
				require.ErrorContains(t, h.execute(t.Context()), "admission")
			}

			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, source.Data[registryauth.ConfigKey])

			for _, call := range *calls {
				require.NotContains(t, call, "role assignment create")
			}
		})
	}
}
