// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/runtime"
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

func TestLateACRAdditionPlansAuthenticationWithoutChangingSource(t *testing.T) {
	source := authSource("")
	env := testEnv(t, source)
	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Ready)
	require.Equal(t, "RegistryAuthInstalling", result.Reason)
	require.Positive(t, result.RequeueAfter)
	require.Len(t, operations, 6)

	var ds appsv1.DaemonSet

	for _, op := range operations {
		if op.Object.GetKind() == "DaemonSet" {
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds))
			require.Len(t, op.DependsOn, 5)
		}
	}

	require.Equal(t, map[string]string{"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue}, ds.Spec.Template.Spec.NodeSelector, "must exclude native AKS nodes")
	require.True(t, ds.Spec.Template.Spec.HostPID)
	require.Equal(t, installationManagerOperator, ds.Labels["app.kubernetes.io/managed-by"])
	require.Equal(t, env.Config.RegistryAuthNodeImage(), ds.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, registryAuthName, ds.Spec.Template.Spec.ServiceAccountName)

	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}

		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				t.Fatal("installer must not project an authentication token")
			}
		}
	}

	require.Equal(t, []string{"registry-auth-node"}, ds.Spec.Template.Spec.Containers[0].Args)
	require.Equal(t, int32(1), ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal)
	require.Equal(t, []corev1.Capability{"SYS_CHROOT"}, ds.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add)

	var got corev1.ConfigMap
	require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &got))
	require.Equal(t, source.Data, got.Data)
}

func componentKey(name string) struct{ Namespace, Name string } {
	return struct{ Namespace, Name string }{Namespace: component.DefaultNamespace, Name: name}
}

func TestNodeAuthInstallationModes(t *testing.T) {
	for _, tc := range []struct {
		name, policy, upstream string
		install                bool
	}{
		{name: "external", policy: "mode: external", upstream: acrUpstream},
		{name: "shared", policy: "mode: external", upstream: acrUpstream + "    credentials_path: /etc/gantry/registry/shared\n"},
		{name: "non-ACR", policy: "mode: external", upstream: strings.ReplaceAll(acrUpstream, "example.azurecr.io", "registry.example.com")},
		{name: "no policy", policy: "mode: external", upstream: acrUpstream},
		{name: "racer", upstream: acrUpstream + "storage_mode: racer\n", install: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := authSource(tc.policy)
			if tc.name == "no policy" {
				delete(source.Data, registryauth.ConfigKey)
			}

			source.Data["config.yaml"] = tc.upstream
			operations, result, err := planRegistryAuth(t.Context(), testEnv(t, source))
			require.NoError(t, err)

			if tc.install {
				require.Len(t, operations, 6, "kubelet authentication must survive a Gantry storage-backend change")
				require.NotNil(t, result)
			} else {
				require.Empty(t, operations)
				require.Nil(t, result)
			}
		})
	}
}

func TestNodeAuthReadinessDistinguishesUnverifiedPermissions(t *testing.T) {
	for _, scheduled := range []int32{0, 3} {
		t.Run(fmt.Sprint(scheduled), func(t *testing.T) {
			source := authSource("")
			env := testEnv(t, source)
			operations, _, err := planRegistryAuth(t.Context(), env)
			require.NoError(t, err)

			var existing appsv1.DaemonSet

			for _, op := range operations {
				if op.Object.GetKind() == "DaemonSet" {
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &existing))
				}
			}

			existing.Generation = 2
			existing.UID = "owned-installer"
			existing.Status = appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: scheduled, UpdatedNumberScheduled: scheduled, NumberAvailable: scheduled}
			require.NoError(t, env.Client.Create(t.Context(), &existing))
			_, result, err := planRegistryAuth(t.Context(), env)
			require.NoError(t, err)
			require.True(t, result.Ready)

			require.Contains(t, result.Message, "unverified")

			existing.Spec.Template.Annotations[registryAuthHash] = "old-config"
			require.NoError(t, env.Client.Update(t.Context(), &existing))
			_, result, err = planRegistryAuth(t.Context(), env)
			require.NoError(t, err)
			require.False(t, result.Ready, "old available pods must not prove the new config is active")
		})
	}
}

func TestNodeAuthExternalModeKeepsReconcilerForOwnedFileCleanup(t *testing.T) {
	source := authSource("mode: external")
	existing := &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()}
	env := testEnv(t, source, existing)
	require.NoError(t, rbacv1.AddToScheme(env.Client.Scheme()))
	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.False(t, result.Ready)
	require.Len(t, operations, 6)

	for _, op := range operations {
		if op.Object.GetKind() == "ConfigMap" {
			var config corev1.ConfigMap
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &config))

			var cfg registryauth.Config
			require.NoError(t, json.Unmarshal([]byte(config.Data["config.json"]), &cfg))
			require.Equal(t, "external", cfg.Mode)
		}
	}
}

func TestNodeAuthLeasePrivilegesAreNarrowAndOperatorCreatesLease(t *testing.T) {
	env := testEnv(t, authSource(""))
	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)

	var role rbacv1.Role

	for _, op := range operations {
		switch op.Object.GetKind() {
		case "Role":
			require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &role))
		case "Lease":
			require.Equal(t, registryauth.RestartLeaseName, op.Object.GetName())
			require.Equal(t, component.OpCreateIfAbsent, op.Kind)
		}
	}

	require.Len(t, role.Rules, 1)
	require.Equal(t, []string{"get", "update"}, role.Rules[0].Verbs)
	require.Equal(t, []string{registryauth.RestartLeaseName}, role.Rules[0].ResourceNames)
	require.Equal(t, []string{"leases"}, role.Rules[0].Resources)
	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("flex", nil)})
	require.NoError(t, err)
	order, err := plan.ExecutionOrder()
	require.NoError(t, err)
	require.Less(t, strings.Index(order, "CreateIfAbsent Lease/"+component.DefaultNamespace+"/"+registryauth.RestartLeaseName), strings.Index(order, "CreateIfAbsent DaemonSet/"+component.DefaultNamespace+"/"+registryAuthName))
}

func TestAuthCleanupRecreatesDeletedHelper(t *testing.T) {
	source := authSource("mode: external")
	retained := &corev1.ConfigMap{ObjectMeta: ownedAuthMetadata()}
	env := testEnv(t, source, retained)
	require.NoError(t, rbacv1.AddToScheme(env.Client.Scheme()))
	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.Len(t, operations, 6)
	require.NotNil(t, result)
	require.False(t, result.Ready)
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

			operations, _, err := planRegistryAuth(t.Context(), env)
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

func TestInvalidAuthPolicyStopsPlanning(t *testing.T) {
	env := testEnv(t, authSource("mode: unsupported"))
	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("flex", nil)})
	require.ErrorContains(t, err, "mode must")
	require.Nil(t, plan)
}
