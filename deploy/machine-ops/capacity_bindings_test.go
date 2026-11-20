// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// renderMachineOps renders every template of this directory with data and
// returns the non-empty YAML documents of each rendered file.
func renderMachineOps(t *testing.T, data map[string]string) map[string][]string {
	t.Helper()

	outputDir := t.TempDir()
	require.NoError(t, render.Render(".", outputDir, data))

	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)

	rendered := map[string][]string{}

	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(outputDir, entry.Name()))
		require.NoError(t, err)

		rendered[entry.Name()] = nil

		for document := range strings.SplitSeq(string(content), "\n---") {
			var typeMeta metav1.TypeMeta
			require.NoError(t, yaml.Unmarshal([]byte(document), &typeMeta), entry.Name())

			if typeMeta.Kind != "" {
				rendered[entry.Name()] = append(rendered[entry.Name()], document)
			}
		}
	}

	return rendered
}

// capacityBinding is one binding of 08-capacity-bindings.yaml.tmpl:
// "ClusterRoleBinding" or "RoleBinding <namespace>", and the ClusterRole.
type capacityBinding struct {
	scope       string
	clusterRole string
}

func capacityBindings(t *testing.T, documents []string, subject rbacv1.Subject) map[string]capacityBinding {
	t.Helper()

	bindings := map[string]capacityBinding{}

	for _, document := range documents {
		// A RoleBinding has every field of a ClusterRoleBinding and a
		// namespace.
		var binding rbacv1.RoleBinding
		require.NoError(t, yaml.UnmarshalStrict([]byte(document), &binding))
		require.Equal(t, rbacv1.SchemeGroupVersion.String(), binding.APIVersion)
		require.Equal(t, []rbacv1.Subject{subject}, binding.Subjects, binding.Name)
		require.Equal(t, rbacv1.GroupName, binding.RoleRef.APIGroup, binding.Name)
		require.Equal(t, "ClusterRole", binding.RoleRef.Kind, binding.Name)

		scope := binding.Kind

		switch binding.Kind {
		case "ClusterRoleBinding":
			require.Empty(t, binding.Namespace, binding.Name)
		case "RoleBinding":
			require.NotEmpty(t, binding.Namespace, binding.Name)
			scope += " " + binding.Namespace
		default:
			t.Fatalf("%s is a %s, not a binding", binding.Name, binding.Kind)
		}

		require.NotContains(t, bindings, binding.Name)
		bindings[binding.Name] = capacityBinding{scope: scope, clusterRole: binding.RoleRef.Name}
	}

	return bindings
}

func controllerDeployment(t *testing.T, rendered map[string][]string) *appsv1.Deployment {
	t.Helper()

	require.Len(t, rendered["03-deployment.yaml"], 1)

	var deployment appsv1.Deployment
	require.NoError(t, yaml.UnmarshalStrict([]byte(rendered["03-deployment.yaml"][0]), &deployment))
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)

	return &deployment
}

func controllerServiceAccount(t *testing.T, rendered map[string][]string) *corev1.ServiceAccount {
	t.Helper()

	require.Len(t, rendered["01-serviceaccount.yaml"], 1)

	var serviceAccount corev1.ServiceAccount
	require.NoError(t, yaml.UnmarshalStrict([]byte(rendered["01-serviceaccount.yaml"][0]), &serviceAccount))

	return &serviceAccount
}

// reusableCapacityRoles returns the names of the ClusterRoles the reusable
// role files define.
func reusableCapacityRoles(t *testing.T, rendered map[string][]string) []string {
	t.Helper()

	var names []string

	for _, file := range []string{"04-machineclass-rbac.yaml", "05-machineallocation-rbac.yaml"} {
		require.NotEmpty(t, rendered[file], file)

		for _, document := range rendered[file] {
			var clusterRole rbacv1.ClusterRole
			require.NoError(t, yaml.UnmarshalStrict([]byte(document), &clusterRole))
			require.Equal(t, "ClusterRole", clusterRole.Kind)

			names = append(names, clusterRole.Name)
		}
	}

	return names
}

// Without EnableCapacity the manifests are what they were before the
// capacity controllers existed: no capacity flag, no binding of a capacity
// role, no workload identity.
func TestCapacityIsOffByDefault(t *testing.T) {
	t.Parallel()

	for name, data := range map[string]map[string]string{
		"no values":      {},
		"explicit false": {"EnableCapacity": "false", "AllocationConsumerKinds": "karpenter.sh/v1/NodeClaim", "ProviderConfigNamespace": "capacity-config"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := renderMachineOps(t, data)

			require.Contains(t, rendered, "08-capacity-bindings.yaml")
			require.Empty(t, rendered["08-capacity-bindings.yaml"], "no capacity role is bound")

			deployment := controllerDeployment(t, rendered)
			for _, arg := range deployment.Spec.Template.Spec.Containers[0].Args {
				require.NotContains(t, arg, "capacity")
				require.NotContains(t, arg, "provider-config-namespace")
				require.NotContains(t, arg, "allocation-consumer-kind")
			}

			require.NotContains(t, deployment.Spec.Template.Labels, "azure.workload.identity/use")
			require.Empty(t, controllerServiceAccount(t, rendered).Annotations)

			// No other file binds a capacity role either.
			roles := reusableCapacityRoles(t, rendered)
			for _, document := range rendered["02-rbac.yaml"] {
				for _, role := range roles {
					require.NotContains(t, document, role)
				}
			}
		})
	}
}

// With EnableCapacity every reusable capacity role is bound to the controller
// ServiceAccount exactly once, at the scope its file prescribes, and nothing
// else is bound.
func TestCapacityBindsEveryReusableRoleOnce(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		data                    map[string]string
		controllerName          string
		namespace               string
		providerConfigNamespace string
	}{
		"defaults": {
			data:                    map[string]string{"EnableCapacity": "true"},
			controllerName:          "azure-machine-ops-controller",
			namespace:               "unbounded-system",
			providerConfigNamespace: "unbounded-system",
		},
		"scoped deployment with its own provider-config namespace": {
			data: map[string]string{
				"EnableCapacity":          "true",
				"ControllerName":          "machine-ops-azure-site-a",
				"Namespace":               "machine-ops",
				"ProviderConfigNamespace": "capacity-config",
			},
			controllerName:          "machine-ops-azure-site-a",
			namespace:               "machine-ops",
			providerConfigNamespace: "capacity-config",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rendered := renderMachineOps(t, test.data)
			subject := rbacv1.Subject{Kind: "ServiceAccount", Name: test.controllerName, Namespace: test.namespace}

			require.Equal(t, test.controllerName, controllerServiceAccount(t, rendered).Name)
			require.Equal(t, test.namespace, controllerServiceAccount(t, rendered).Namespace)

			prefix := test.controllerName + "-"
			want := map[string]capacityBinding{
				prefix + "machineclass":                 {scope: "ClusterRoleBinding", clusterRole: "unbounded-machineclass-controller"},
				prefix + "machineclass-provider-config": {scope: "RoleBinding " + test.providerConfigNamespace, clusterRole: "unbounded-machineclass-controller-provider-config"},
				prefix + "machineclass-credentials":     {scope: "RoleBinding " + test.namespace, clusterRole: "unbounded-machineclass-controller-credentials"},
				prefix + "machineallocation":            {scope: "ClusterRoleBinding", clusterRole: "unbounded-machineallocation-controller"},
				prefix + "machineallocation-provider-config": {
					scope: "RoleBinding " + test.providerConfigNamespace, clusterRole: "unbounded-machineallocation-controller-provider-config",
				},
				prefix + "machineallocation-bootstrap-tokens": {
					scope: "RoleBinding kube-system", clusterRole: "unbounded-machineallocation-controller-bootstrap-tokens",
				},
				prefix + "machineallocation-operations": {scope: "ClusterRoleBinding", clusterRole: "unbounded-machineallocation-operations"},
				prefix + "machineallocation-operations-bootstrap-tokens": {
					scope: "RoleBinding kube-system", clusterRole: "unbounded-machineallocation-operations-bootstrap-tokens",
				},
			}

			got := capacityBindings(t, rendered["08-capacity-bindings.yaml"], subject)
			require.Equal(t, want, got)

			// Every reusable role is bound, and every bound role exists.
			var bound []string
			for _, binding := range got {
				bound = append(bound, binding.clusterRole)
			}

			require.ElementsMatch(t, reusableCapacityRoles(t, rendered), bound)

			// Roles that read or write ConfigMaps and Secrets are never
			// bound cluster-wide.
			for bindingName, binding := range got {
				if strings.Contains(binding.clusterRole, "provider-config") ||
					strings.Contains(binding.clusterRole, "credentials") ||
					strings.Contains(binding.clusterRole, "bootstrap-tokens") {
					require.True(t, strings.HasPrefix(binding.scope, "RoleBinding "), bindingName)
				}
			}

			// The admission policies the allocation controller needs render
			// with the bindings.
			require.Len(t, rendered["06-machine-allocationref-policy.yaml"], 2)
			require.Len(t, rendered["07-machineallocation-bootstrap-token-policy.yaml"], 2)

			args := controllerDeployment(t, rendered).Spec.Template.Spec.Containers[0].Args
			require.Contains(t, args, "--enable-capacity")
			require.Contains(t, args, "--leader-elect=true")
			require.Contains(t, args, "--provider-config-namespace="+test.providerConfigNamespace)
			require.Contains(t, args, "--credential-secret-namespace="+test.namespace)

			for _, arg := range args {
				require.NotContains(t, arg, "allocation-consumer-kind", "consumer kinds are empty unless asked for")
			}
		})
	}
}

func TestCapacityConsumerKindsBecomeRepeatedFlags(t *testing.T) {
	t.Parallel()

	rendered := renderMachineOps(t, map[string]string{
		"EnableCapacity":          "true",
		"AllocationConsumerKinds": "karpenter.sh/v1/NodeClaim,example.io/v1/Claim",
	})

	args := controllerDeployment(t, rendered).Spec.Template.Spec.Containers[0].Args
	require.Contains(t, args, "--allocation-consumer-kind=karpenter.sh/v1/NodeClaim")
	require.Contains(t, args, "--allocation-consumer-kind=example.io/v1/Claim")
}

func TestWorkloadIdentityIsOptIn(t *testing.T) {
	t.Parallel()

	rendered := renderMachineOps(t, map[string]string{"WorkloadIdentityClientID": "11111111-2222-3333-4444-555555555555"})

	require.Equal(t, map[string]string{
		"azure.workload.identity/client-id": "11111111-2222-3333-4444-555555555555",
	}, controllerServiceAccount(t, rendered).Annotations)

	deployment := controllerDeployment(t, rendered)
	require.Equal(t, "true", deployment.Spec.Template.Labels["azure.workload.identity/use"])
	require.NotContains(t, deployment.Spec.Selector.MatchLabels, "azure.workload.identity/use")
}
