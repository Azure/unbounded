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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/registryauth"
)

const (
	attachSubscription = "11111111-2222-3333-4444-555555555555"
	attachACRID        = "/subscriptions/" + attachSubscription + "/resourceGroups/registry-rg/providers/Microsoft.ContainerRegistry/registries/central"
	attachClusterID    = "/subscriptions/" + attachSubscription + "/resourceGroups/cluster-rg/providers/Microsoft.ContainerService/managedClusters/cluster"
	attachIdentityID   = "/subscriptions/" + attachSubscription + "/resourceGroups/cluster-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/pull"
	attachClientID     = "22222222-2222-3333-4444-555555555555"
	attachTenantID     = "33333333-2222-3333-4444-555555555555"
	attachPrincipalID  = "44444444-2222-3333-4444-555555555555"
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
	h.runAzure = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch strings.Join(args[:min(2, len(args))], " ") {
		case "acr show":
			require.Equal(t, []string{"acr", "show", "--name", "central", "--resource-group", "registry-rg"}, args)

			return json.Marshal(map[string]string{"id": attachACRID, "loginServer": "central.azurecr.io", "roleAssignmentMode": "LegacyRegistryPermissions"})
		case "aks show":
			require.Equal(t, []string{"aks", "show", "--name", "cluster", "--resource-group", "cluster-rg"}, args)

			return json.Marshal(map[string]any{
				"id": attachClusterID, "location": "eastus", "fqdn": "cluster.example.com", "provisioningState": "Succeeded",
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
				"id": attachIdentityID, "clientId": attachClientID, "tenantId": attachTenantID, "principalId": attachPrincipalID,
				"tags": map[string]string{"unbounded-cluster-id": attachClusterID},
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

			return json.Marshal(map[string]any{"properties": map[string]any{"managedIdentity": map[string]string{"resourceId": attachIdentityID}, "provisioningState": "Succeeded"}})
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

func managedAttachFixture(t *testing.T) (*attachACRHandler, *fake.Clientset, *[]string) {
	t.Helper()
	h, client, calls := attachFixture(t)
	h.authMode = registryauth.IdentityBindingMode
	ca := attachTestCA(t)
	_, err := client.CoreV1().ConfigMaps(h.namespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-ca", Namespace: h.namespace}, Data: map[string]string{"ca.crt": ca},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
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
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "proxy-ca"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca-cert/ca.crt"}}}},
			},
		}}}}

		return true, pod, nil
	})

	cfg := registryauth.Config{
		Mode: h.authMode, Registry: "central.azurecr.io", ClientID: attachClientID,
		TenantID: attachTenantID, SNIName: "proxy.example.com", ProxyAddress: "10.0.0.1", Audience: "api://binding", CACert: ca,
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	hash := sha256.Sum256(data)
	_, err = client.AppsV1().DaemonSets(h.namespace).Create(t.Context(), &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "gantry-acr-auth", Namespace: h.namespace, Generation: 1},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"unbounded-cloud.io/registry-auth-config-hash": hex.EncodeToString(hash[:])},
		}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberAvailable: 1},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "off-azure", Labels: map[string]string{registryauth.FlexNodeLabel: registryauth.FlexNodeValue, "kubernetes.io/os": "linux"}},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.0"}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	client.ClearActions()

	return h, client, calls
}

func TestGantryAttachACRDefaultsToExternalAuth(t *testing.T) {
	cmd := gantryCommandGroup()
	attach, _, err := cmd.Find([]string{"attach-acr"})
	require.NoError(t, err)
	require.Equal(t, registryauth.ExternalMode, attach.Flags().Lookup("auth-mode").DefValue)
	require.NotContains(t, attach.Flags().Lookup("auth-mode").Annotations, "cobra_annotation_bash_completion_one_required_flag")
	cmd.SetArgs([]string{"attach-acr"})
	require.ErrorContains(t, cmd.Execute(), `"acr-resource-id"`)
}

func TestGantryAttachACRExternalPreservesCustomerAuthentication(t *testing.T) {
	for _, mode := range []string{"", registryauth.ExternalMode} {
		t.Run("mode="+mode, func(t *testing.T) {
			h, client, calls := attachFixture(t)
			h.authMode = mode
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			source.Data[registryauth.ConfigKey] = "customer-owned-policy: untouched"
			source.Data["config.yaml"] += "  credentials_path: /shared/auth\n"
			_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
			require.NoError(t, err)
			client.ClearActions()
			require.NoError(t, h.execute(t.Context()))
			require.Len(t, *calls, 1)
			require.Contains(t, (*calls)[0], "acr show")
			got, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), source.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, source.Data[registryauth.ConfigKey], got.Data[registryauth.ConfigKey])
			require.Equal(t, "preserve", got.Data["other.yaml"])
			require.Contains(t, got.Data["config.yaml"], "# retain comment")
			require.Contains(t, got.Data["config.yaml"], "credentials_path: /shared/auth")
			require.Contains(t, got.Data["config.yaml"], "central.azurecr.io")
			require.Contains(t, h.out.(*bytes.Buffer).String(), "existing authentication unchanged")
			require.NotContains(t, h.out.(*bytes.Buffer).String(), "WARNING:")

			for _, action := range client.Actions() {
				require.Equal(t, "configmaps", action.GetResource().Resource)
			}

			require.NoError(t, h.execute(t.Context()))
		})
	}
}

func TestGantryManagedACRWarningPrecedesEverySideEffect(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
		require.Contains(t, h.out.(*bytes.Buffer).String(), "kubelets on all targeted Linux Flex nodes concurrently")
		require.Contains(t, h.out.(*bytes.Buffer).String(), "Nodes are not drained automatically")
		require.Contains(t, h.out.(*bytes.Buffer).String(), "responsible for the impact on existing workloads")

		return run(ctx, args...)
	}

	client.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		require.Contains(t, h.out.(*bytes.Buffer).String(), "WARNING:")
		return false, nil, nil
	})
	require.NoError(t, h.execute(t.Context()))
}

func TestGantryAttachACRDefaultDoesNotCreateAuthenticationPolicy(t *testing.T) {
	h, client, calls := attachFixture(t)
	h.authMode = ""
	require.NoError(t, h.execute(t.Context()))
	require.Len(t, *calls, 1)

	var source *corev1.ConfigMap

	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, source.Data, registryauth.ConfigKey)
	require.Contains(t, source.Data["config.yaml"], "central.azurecr.io")

	for _, action := range client.Actions() {
		require.Equal(t, "configmaps", action.GetResource().Resource)
	}
}

func TestGantryAttachACRDefaultPreservesPreviouslyManagedAuthentication(t *testing.T) {
	h, client, calls := managedAttachFixture(t)
	require.NoError(t, h.execute(t.Context()))
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)

	policy := source.Data[registryauth.ConfigKey]
	h.authMode = ""
	*calls = nil

	client.ClearActions()
	require.NoError(t, h.execute(t.Context()))
	require.Len(t, *calls, 1)
	source, err = client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, policy, source.Data[registryauth.ConfigKey])

	for _, action := range client.Actions() {
		require.Equal(t, "configmaps", action.GetResource().Resource)
	}
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

func TestGantryAttachACRPublishesIdentityGrantAfterManagedInstallation(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		// A queued external-policy reconciliation can revoke a staged grant.
		err := client.Tracker().Delete(rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings"), "", registryauth.IdentityRoleName)
		require.True(t, err == nil || apierrors.IsNotFound(err))

		return false, nil, nil
	})
	require.NoError(t, h.execute(t.Context()))
	_, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err, "the identity-use grant must survive the transition from external policy")

	installerRead, grantWrite := -1, -1

	for index, action := range client.Actions() {
		if action.Matches("get", "daemonsets") {
			installerRead = index
		}

		if action.Matches("create", "clusterrolebindings") {
			grantWrite = index
		}
	}

	require.NotEqual(t, -1, installerRead)
	require.Greater(t, grantWrite, installerRead, "wait for the operator to observe managed policy before publishing the grant")
}

func TestGantryAttachACRDoesNotStageIdentityGrantWhenPolicySaveFails(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("policy write failed")
	})
	require.ErrorContains(t, h.execute(t.Context()), "policy write failed")
	_, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "a failed opt-in must not publish collective identity-use permission")
}

func TestGantryAttachACRRejectsConcurrentInvalidEndpoint(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "identity" && args[1] == "create" {
			source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
			require.NoError(t, err)

			source.Data["config.yaml"] += "- name: central.azurecr.io\n  endpoint: http://central.azurecr.io\n"
			_, err = client.CoreV1().ConfigMaps(h.namespace).Update(t.Context(), source, metav1.UpdateOptions{})
			require.NoError(t, err)
		}

		return run(ctx, args...)
	}
	require.ErrorContains(t, h.execute(t.Context()), "matching HTTPS endpoint")
	source, err := client.CoreV1().ConfigMaps(h.namespace).Get(t.Context(), "gantry-config", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, source.Data, registryauth.ConfigKey, "invalid merged policy must not be persisted")
}

func TestGantryIdentityUseGrantToleratesConcurrentControllerCreate(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	raced := false

	client.PrependReactor("create", "clusterroles", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil
		}

		raced = true
		create, ok := action.(k8stesting.CreateAction)
		require.True(t, ok)
		require.NoError(t, client.Tracker().Create(rbacv1.SchemeGroupVersion.WithResource("clusterroles"), create.GetObject(), ""))

		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, registryauth.IdentityRoleName)
	})
	require.NoError(t, h.grantACRIdentityUse(t.Context(), attachClientID))
	_, err := client.RbacV1().ClusterRoleBindings().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestGantryIdentityUseRepairClearsAggregation(t *testing.T) {
	h, client, _ := managedAttachFixture(t)
	require.NoError(t, h.grantACRIdentityUse(t.Context(), attachClientID))
	role, err := client.RbacV1().ClusterRoles().Get(t.Context(), registryauth.IdentityRoleName, metav1.GetOptions{})
	require.NoError(t, err)

	role.AggregationRule = &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{}}}
	_, err = client.RbacV1().ClusterRoles().Update(t.Context(), role, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.grantACRIdentityUse(t.Context(), attachClientID))
	role, err = client.RbacV1().ClusterRoles().Get(t.Context(), role.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Nil(t, role.AggregationRule)
	require.Equal(t, []string{attachClientID}, role.Rules[0].Resources)
}

func TestGantryAttachACRRejectsWrongClusterBeforeAzureWrites(t *testing.T) {
	h, _, calls := managedAttachFixture(t)
	h.restConfig.Host = "https://different.example.com"
	require.ErrorContains(t, h.execute(t.Context()), "does not match")
	require.Len(t, *calls, 2)
}

func TestGantryAttachACREnablesPrerequisitesWithNamedCluster(t *testing.T) {
	h, _, calls := managedAttachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := run(ctx, args...)
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
}

func TestGantryAttachACRManagedRerunsReuseOwnedResources(t *testing.T) {
	h, _, calls := managedAttachFixture(t)
	require.NoError(t, h.execute(t.Context()))

	digest := sha256.Sum256([]byte(strings.ToLower(attachClusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])
	run := h.runAzure
	h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
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
			return run(ctx, args...)
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

func TestGantryAttachACRDoesNotBroadenInheritedConditionalPullGrant(t *testing.T) {
	h, _, calls := attachFixture(t)
	run := h.runAzure
	h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.HasPrefix(strings.Join(args, " "), "role assignment list") {
			require.Contains(t, args, "--include-inherited")

			return json.Marshal([]map[string]string{{
				"principalId": attachPrincipalID, "scope": "/subscriptions/" + attachSubscription,
				"roleDefinitionId": "/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d", "condition": "repository restriction",
			}})
		}

		return run(ctx, args...)
	}
	require.ErrorContains(t, h.ensureACRPullGrant(t.Context(), attachPrincipalID, "7f951dda-4ed3-4680-a7ca-43fe172d538d"), "conditional")
	require.Empty(t, *calls)
}

func TestGantryAttachACRRejectsOldKubeletsAndCrossSubscription(t *testing.T) {
	h, client, calls := managedAttachFixture(t)
	node, err := client.CoreV1().Nodes().Get(t.Context(), "off-azure", metav1.GetOptions{})
	require.NoError(t, err)

	node.Status.NodeInfo.KubeletVersion = "v1.33.9"
	_, err = client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, h.execute(t.Context()), "1.34")
	require.Len(t, *calls, 2)
	*calls = nil
	h.clusterID = strings.Replace(attachClusterID, attachSubscription, attachClientID, 1)
	require.ErrorContains(t, h.execute(t.Context()), "same subscription")
	require.Empty(t, *calls)
}

func TestGantryAttachACRRejectsMissingAdmissionAndConditionalGrants(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		t.Run(fmt.Sprint(conditional), func(t *testing.T) {
			h, client, calls := managedAttachFixture(t)
			if conditional {
				run := h.runAzure
				h.runAzure = func(ctx context.Context, args ...string) ([]byte, error) {
					if strings.HasPrefix(strings.Join(args, " "), "role assignment list") {
						return json.Marshal([]map[string]string{{
							"principalId": attachPrincipalID, "scope": attachACRID,
							"roleDefinitionId": "/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d", "condition": "repository restriction",
						}})
					}

					return run(ctx, args...)
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
