// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const acrUpstream = "upstream_registries:\n  - name: example.azurecr.io\n    endpoint: https://example.azurecr.io\n"

func authSource(policy string) *corev1.ConfigMap {
	if !strings.HasPrefix(policy, "mode:") {
		policy = "mode: workloadIdentity\nclientID: 11111111-2222-3333-4444-555555555555\ntenantID: aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\n" + policy
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
	require.Equal(t, env.Config.RegistryAuthNodeImage(), ds.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, registryauth.ServiceAccountName, ds.Spec.Template.Spec.ServiceAccountName)

	var projected bool

	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}

		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				require.Equal(t, registryauth.FederatedAudience, source.ServiceAccountToken.Audience)

				projected = true
			}
		}
	}

	require.True(t, projected, "the broker needs its own assertion, not an application's token")
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
		{name: "shared", upstream: acrUpstream + "    credentials_path: /etc/gantry/registry/shared\n"},
		{name: "non-ACR", upstream: strings.ReplaceAll(acrUpstream, "example.azurecr.io", "registry.example.com")},
		{name: "racer", upstream: acrUpstream + "storage_mode: racer\n", install: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := authSource(tc.policy)
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
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverified", true: "verified"}[probe], func(t *testing.T) {
			policy := ""
			if probe {
				policy = "probeImage: example.azurecr.io/private/image:v1"
			}

			source := authSource(policy)
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
			existing.Status = appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3}
			require.NoError(t, env.Client.Create(t.Context(), &existing))
			_, result, err := planRegistryAuth(t.Context(), env)
			require.NoError(t, err)
			require.True(t, result.Ready)

			if probe {
				require.Contains(t, result.Message, "authorization verified")
			} else {
				require.Contains(t, result.Message, "permission is unverified")
			}

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
	existing := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: component.DefaultNamespace}}
	operations, result, err := planRegistryAuth(t.Context(), testEnv(t, source, existing))
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
	require.Less(t, strings.Index(order, "CreateIfAbsent Lease/"+component.DefaultNamespace+"/"+registryauth.RestartLeaseName), strings.Index(order, "Apply DaemonSet/"+component.DefaultNamespace+"/"+registryAuthName))
}

func TestAuthCleanupRecreatesDeletedHelper(t *testing.T) {
	source := authSource("mode: external")
	retained := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: component.DefaultNamespace}}
	operations, result, err := planRegistryAuth(t.Context(), testEnv(t, source, retained))
	require.NoError(t, err)
	require.Len(t, operations, 6)
	require.NotNil(t, result)
	require.False(t, result.Ready)
}

func TestInvalidAuthPolicyStopsPlanning(t *testing.T) {
	env := testEnv(t, authSource("mode: unsupported"))
	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("flex", nil)})
	require.ErrorContains(t, err, "mode must")
	require.Nil(t, plan)
}
