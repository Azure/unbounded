// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rebound

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func renderObjects(t *testing.T, values map[string]string) map[string]*unstructured.Unstructured {
	t.Helper()

	out := t.TempDir()
	require.NoError(t, render.Render(".", out, values))

	files, err := filepath.Glob(filepath.Join(out, "*.yaml"))
	require.NoError(t, err)
	require.Len(t, files, 8)

	objects := map[string]*unstructured.Unstructured{}

	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)

		decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err == io.EOF {
				break
			} else {
				require.NoError(t, err)
			}

			key := obj.GetKind() + "/" + obj.GetName()
			require.NotContains(t, objects, key)
			objects[key] = obj
		}
	}

	return objects
}

func decodeObject[T any](t *testing.T, objects map[string]*unstructured.Unstructured, key string) T {
	t.Helper()
	require.Contains(t, objects, key)

	var result T
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(objects[key].Object, &result))

	return result
}

func TestRender(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, namespace, image, ttl, maxNodes, excluded string
		replicas                                        int32
		values                                          map[string]string
	}{
		{"defaults", "unbounded-system", "ghcr.io/azure/rebound:latest", "1h", "500", "kube-system,unbounded-system", 2, nil},
		{"overrides", "grants", "example.test/rebound:test", "30m", "25", "platform,grants", 3, map[string]string{
			"Namespace": "grants", "Image": "example.test/rebound:test", "MaxTTL": "30m", "Replicas": "3",
			"MaxNodesPerGrant":   "25",
			"ExcludedNamespaces": "platform,grants", "AdminGroups": "operators,admins",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := renderObjects(t, tc.values)
			require.Len(t, objects, 18)
			require.Contains(t, objects, "Namespace/"+tc.namespace)
			require.Contains(t, objects, "ServiceAccount/rebound")
			require.NotContains(t, objects, "ConfigMap/rebound-ledger")

			for _, obj := range objects {
				if obj.GetNamespace() != "" && obj.GetName() != "rebound-authentication-reader" {
					require.Equal(t, tc.namespace, obj.GetNamespace())
				}
			}

			deployment := decodeObject[appsv1.Deployment](t, objects, "Deployment/rebound")
			require.Equal(t, tc.replicas, *deployment.Spec.Replicas)
			pod := deployment.Spec.Template.Spec
			require.Equal(t, "rebound", pod.ServiceAccountName)
			require.True(t, *pod.SecurityContext.RunAsNonRoot)
			require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
			require.Equal(t, "kubernetes.io/hostname", pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].PodAffinityTerm.TopologyKey)
			require.Len(t, pod.Containers, 1)
			container := pod.Containers[0]
			require.Equal(t, tc.image, container.Image)
			require.ElementsMatch(t, []string{
				"--namespace=$(POD_NAMESPACE)", "--max-ttl=" + tc.ttl,
				"--max-nodes-per-grant=" + tc.maxNodes,
				"--secure-port=8443", "--metrics-bind-address=:8080", "--health-probe-bind-address=:8081", "--leader-elect=true",
				"--excluded-namespaces=" + tc.excluded,
			}, container.Args)
			require.Equal(t, "POD_NAMESPACE", container.Env[0].Name)
			require.Equal(t, "metadata.namespace", container.Env[0].ValueFrom.FieldRef.FieldPath)
			require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
			require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
			require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
			require.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
			require.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)
			require.Equal(t, intstr.FromString("https"), container.ReadinessProbe.HTTPGet.Port)
			require.Equal(t, corev1.URISchemeHTTPS, container.ReadinessProbe.HTTPGet.Scheme)
			require.Contains(t, container.Ports, corev1.ContainerPort{Name: "health", ContainerPort: 8081})

			service := decodeObject[corev1.Service](t, objects, "Service/rebound")
			require.EqualValues(t, 443, service.Spec.Ports[0].Port)
			require.Equal(t, intstr.FromInt32(8443), service.Spec.Ports[0].TargetPort)
			require.Equal(t, deployment.Spec.Selector.MatchLabels, service.Spec.Selector)
			pdb := decodeObject[policyv1.PodDisruptionBudget](t, objects, "PodDisruptionBudget/rebound")
			require.Equal(t, intstr.FromInt32(1), *pdb.Spec.MinAvailable)
			require.Equal(t, deployment.Spec.Selector, pdb.Spec.Selector)

			api := objects["APIService/v1alpha1.rebound.unbounded-cloud.io"]
			require.NotNil(t, api)
			require.Equal(t, map[string]any{
				"group": "rebound.unbounded-cloud.io", "version": "v1alpha1", "groupPriorityMinimum": int64(1000),
				"versionPriority": int64(15), "insecureSkipTLSVerify": false,
				"service": map[string]any{"name": "rebound", "namespace": tc.namespace, "port": int64(443)},
			}, api.Object["spec"])

			for _, name := range []string{"rebound-pod-placement", "rebound-node-keys"} {
				policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/"+name)
				require.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)
				binding := decodeObject[admissionv1.ValidatingAdmissionPolicyBinding](t, objects, "ValidatingAdmissionPolicyBinding/"+name)
				require.Equal(t, name, binding.Spec.PolicyName)
				require.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, binding.Spec.ValidationActions)

				if name == "rebound-pod-placement" {
					require.Equal(t, &admissionv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"}, policy.Spec.ParamKind)
					require.Equal(t, "rebound-ledger", binding.Spec.ParamRef.Name)
					require.Equal(t, tc.namespace, binding.Spec.ParamRef.Namespace)
					require.Equal(t, admissionv1.AllowAction, *binding.Spec.ParamRef.ParameterNotFoundAction)
					require.Equal(t, []string{"pods", "pods/binding", "bindings"}, policy.Spec.MatchConstraints.ResourceRules[0].Resources)
				} else {
					require.Nil(t, binding.Spec.ParamRef)
					require.Equal(t, []string{"nodes", "nodes/status"}, policy.Spec.MatchConstraints.ResourceRules[0].Resources)
				}
			}

			assertRBAC(t, objects, tc.namespace)
		})
	}
}

func TestPodPlacementExcludedNamespaces(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name: "defaults",
			want: `["kube-system","unbounded-system"]`,
		},
		{
			name:   "custom namespace with default exclusions",
			values: map[string]string{"Namespace": "grants"},
			want:   `["kube-system","unbounded-system","grants"]`,
		},
		{
			name:   "custom exclusions with default namespace",
			values: map[string]string{"ExcludedNamespaces": "platform"},
			want:   `["platform","unbounded-system"]`,
		},
		{
			name:   "custom exclusions without rebound namespace",
			values: map[string]string{"Namespace": "grants", "ExcludedNamespaces": "platform"},
			want:   `["platform","grants"]`,
		},
		{
			name:   "duplicate exclusions including rebound namespace",
			values: map[string]string{"Namespace": "grants", "ExcludedNamespaces": "platform,grants,platform,grants"},
			want:   `["platform","grants"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := renderObjects(t, tc.values)
			policy := decodeObject[admissionv1.ValidatingAdmissionPolicy](t, objects, "ValidatingAdmissionPolicy/rebound-pod-placement")
			require.Contains(t, policy.Spec.Variables, admissionv1.Variable{
				Name: "excludedNamespaces", Expression: tc.want,
			})
		})
	}
}

func assertRBAC(t *testing.T, objects map[string]*unstructured.Unstructured, namespace string) {
	t.Helper()

	role := decodeObject[rbacv1.Role](t, objects, "Role/rebound")
	require.Equal(t, namespace, role.Namespace)
	require.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"rebound-ledger"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"rebound-serving"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{"rebound"}, Verbs: []string{"get", "update"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
	}, role.Rules)

	for resource, name := range map[string]string{"configmaps": "rebound-ledger", "secrets": "rebound-serving"} {
		for _, verb := range []string{"get", "list", "watch", "update", "patch"} {
			found := false

			for _, rule := range role.Rules {
				if slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) {
					require.Equal(t, []string{name}, rule.ResourceNames)

					found = true
				}
			}

			if !found {
				t.Fatalf("missing %s rule for %s", verb, resource)
			}
		}
	}

	clusterRole := decodeObject[rbacv1.ClusterRole](t, objects, "ClusterRole/rebound")
	require.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch", "update"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list", "delete"}},
		{APIGroups: []string{"apiregistration.k8s.io"}, Resources: []string{"apiservices"}, ResourceNames: []string{"v1alpha1.rebound.unbounded-cloud.io"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
	}, clusterRole.Rules)

	for _, name := range []string{"rebound", "rebound-auth-delegator"} {
		binding := decodeObject[rbacv1.ClusterRoleBinding](t, objects, "ClusterRoleBinding/"+name)
		require.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "rebound", Namespace: namespace}}, binding.Subjects)

		if name == "rebound-auth-delegator" {
			require.Equal(t, "system:auth-delegator", binding.RoleRef.Name)
		} else {
			require.Equal(t, "rebound", binding.RoleRef.Name)
		}
	}

	reader := decodeObject[rbacv1.RoleBinding](t, objects, "RoleBinding/rebound-authentication-reader")
	require.Equal(t, "kube-system", reader.Namespace)
	require.Equal(t, "extension-apiserver-authentication-reader", reader.RoleRef.Name)
	require.Equal(t, namespace, reader.Subjects[0].Namespace)

	view := decodeObject[rbacv1.ClusterRole](t, objects, "ClusterRole/rebound-nodegrants-view")
	require.Len(t, view.Rules, 1)
	require.Equal(t, []string{"rebound.unbounded-cloud.io"}, view.Rules[0].APIGroups)
	require.Equal(t, []string{"nodegrants"}, view.Rules[0].Resources)
	require.Equal(t, []string{"get", "list", "watch"}, view.Rules[0].Verbs)
	edit := decodeObject[rbacv1.ClusterRole](t, objects, "ClusterRole/rebound-nodegrants-edit")
	require.Len(t, edit.Rules, 1)
	require.Equal(t, []string{"rebound.unbounded-cloud.io"}, edit.Rules[0].APIGroups)
	require.Equal(t, []string{"nodegrants"}, edit.Rules[0].Resources)
	require.Equal(t, []string{"get", "list", "watch", "create", "delete", "deletecollection"}, edit.Rules[0].Verbs)
	require.NotContains(t, edit.Labels, "rbac.authorization.k8s.io/aggregate-to-view")

	for _, target := range []string{"view", "edit", "admin"} {
		require.Equal(t, "true", view.Labels["rbac.authorization.k8s.io/aggregate-to-"+target])

		if target != "view" {
			require.Equal(t, "true", edit.Labels["rbac.authorization.k8s.io/aggregate-to-"+target])
		}
	}
}
