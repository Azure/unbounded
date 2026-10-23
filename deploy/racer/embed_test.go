// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func documents(t *testing.T, file string) []*unstructured.Unstructured {
	t.Helper()

	data, err := fs.ReadFile(Manifests, file)
	require.NoError(t, err)

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	var out []*unstructured.Unstructured

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); errors.Is(err, io.EOF) {
			return out
		} else {
			require.NoError(t, err)
		}

		if obj.Object != nil {
			out = append(out, obj)
		}
	}
}

func convert[T any](t *testing.T, obj *unstructured.Unstructured) *T {
	t.Helper()

	data, err := obj.MarshalJSON()
	require.NoError(t, err)

	out := new(T)
	require.NoError(t, yaml.UnmarshalStrict(data, out))

	return out
}

func TestManifestSet(t *testing.T) {
	entries, err := fs.ReadDir(Manifests, ".")
	require.NoError(t, err)

	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	// No controller Deployment, Service, PDB, config, TLS, or bootstrap trust.
	require.ElementsMatch(t, []string{"control.yaml", "dataplane-config.yaml", "mutating-webhook.yaml", "policies.yaml", "rbac.yaml"}, names)
}

func TestMutatingWebhookManifest(t *testing.T) {
	data, err := fs.ReadFile(Manifests, "mutating-webhook.yaml")
	require.NoError(t, err)

	var config admissionregistrationv1.MutatingWebhookConfiguration
	require.NoError(t, yaml.UnmarshalStrict(data, &config))
	require.Equal(t, "MutatingWebhookConfiguration", config.Kind)
	require.Equal(t, "racer-object-sidecar-injector", config.Name)
	require.Len(t, config.Webhooks, 1)

	hook := config.Webhooks[0]
	require.Equal(t, "racer-object-sidecar.racer.unbounded-cloud.io", hook.Name)
	// The runtime primary owns caBundle. Without it the webhook fails closed.
	require.Empty(t, hook.ClientConfig.CABundle)
	require.Nil(t, hook.ClientConfig.URL)
	require.Equal(t, &admissionregistrationv1.ServiceReference{Name: "racer-webhook", Namespace: "unbounded-system", Path: ptr("/mutate-racer-object-sidecar"), Port: ptr(int32(9443))}, hook.ClientConfig.Service)

	require.Len(t, hook.Rules, 1)
	rule := hook.Rules[0]
	require.Equal(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}, rule.Operations)
	require.Equal(t, []string{""}, rule.APIGroups)
	require.Equal(t, []string{"v1"}, rule.APIVersions)
	require.Equal(t, []string{"pods"}, rule.Resources)
	require.Equal(t, admissionregistrationv1.NamespacedScope, *rule.Scope)

	require.Equal(t, map[string]string{"racer.unbounded-cloud.io/object-sidecar": "enabled"}, hook.ObjectSelector.MatchLabels)
	require.Empty(t, hook.ObjectSelector.MatchExpressions)
	// The dataplane namespace is excluded so the seats that serve this webhook
	// can always start.
	require.Len(t, hook.NamespaceSelector.MatchExpressions, 1)
	require.Equal(t, "kubernetes.io/metadata.name", hook.NamespaceSelector.MatchExpressions[0].Key)
	require.EqualValues(t, "NotIn", hook.NamespaceSelector.MatchExpressions[0].Operator)
	require.Equal(t, []string{"unbounded-system"}, hook.NamespaceSelector.MatchExpressions[0].Values)

	require.Len(t, hook.MatchConditions, 1)
	require.Equal(t, 2, strings.Count(hook.MatchConditions[0].Expression, "c.name == 'racer-object'"))
	require.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy)
	require.Equal(t, admissionregistrationv1.SideEffectClassNone, *hook.SideEffects)
	require.Equal(t, admissionregistrationv1.NeverReinvocationPolicy, *hook.ReinvocationPolicy)
	require.Equal(t, int32(5), *hook.TimeoutSeconds)
	require.Equal(t, []string{"v1"}, hook.AdmissionReviewVersions)
}

func TestControlObjects(t *testing.T) {
	objects := documents(t, "control.yaml")
	require.Len(t, objects, 19)

	service := convert[corev1.Service](t, objects[0])
	require.Equal(t, "racer-webhook", service.Name)
	require.Empty(t, service.Spec.Selector)
	require.Equal(t, []corev1.ServicePort{{Name: "webhook", Port: 9443, TargetPort: intstr.FromInt32(9443), Protocol: corev1.ProtocolTCP}}, service.Spec.Ports)

	secret := convert[corev1.Secret](t, objects[1])
	require.Equal(t, "racer-state", secret.Name)
	require.Equal(t, corev1.SecretTypeOpaque, secret.Type)
	require.Empty(t, secret.Data)
	require.Empty(t, secret.StringData)
	require.Empty(t, secret.Annotations)

	var leases []string

	for _, obj := range objects[2:] {
		lease := convert[coordinationv1.Lease](t, obj)
		require.Equal(t, "unbounded-system", lease.Namespace)
		require.Equal(t, "true", lease.Labels["racer.unbounded-cloud.io/control"])
		require.Nil(t, lease.Spec.HolderIdentity)
		require.Nil(t, lease.Spec.LeaseTransitions)

		leases = append(leases, lease.Name)
	}

	require.ElementsMatch(t, leaseNames(), leases)
}

// leaseNames lists the 17 control Leases.
func leaseNames() []string {
	names := []string{"racer-primary"}
	for seat := range 16 {
		names = append(names, fmt.Sprintf("racer-seat-%d", seat))
	}

	return names
}

func TestRBACGrantsExactly(t *testing.T) {
	objects := documents(t, "rbac.yaml")
	require.Len(t, objects, 4)

	clusterRole := convert[rbacv1.ClusterRole](t, objects[0])
	require.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"racer.unbounded-cloud.io"}, Resources: []string{"clustercaches"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"mutatingwebhookconfigurations"}, ResourceNames: []string{"racer-object-sidecar-injector"}, Verbs: []string{"get", "patch"}},
	}, clusterRole.Rules)

	role := convert[rbacv1.Role](t, objects[2])

	leases := role.Rules[0]
	require.ElementsMatch(t, leaseNames(), leases.ResourceNames)
	leases.ResourceNames = nil
	role.Rules[0] = leases

	require.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "update"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"racer-state"}, Verbs: []string{"get", "update"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch", "patch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, ResourceNames: []string{"racer-dataplane"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, ResourceNames: []string{"racer-dataplane"}, Verbs: []string{"get"}},
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, ResourceNames: []string{"racer-webhook"}, Verbs: []string{"get", "update"}},
	}, role.Rules)

	for _, obj := range []*unstructured.Unstructured{objects[1], objects[3]} {
		subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
		require.NoError(t, err)
		require.Equal(t, []any{map[string]any{"kind": "ServiceAccount", "name": "racer-dataplane", "namespace": "unbounded-system"}}, subjects)
	}
}

func TestPoliciesBindEachPolicy(t *testing.T) {
	objects := documents(t, "policies.yaml")
	require.Len(t, objects, 10)

	for i := 0; i < len(objects); i += 2 {
		policy := convert[admissionregistrationv1.ValidatingAdmissionPolicy](t, objects[i])
		binding := convert[admissionregistrationv1.ValidatingAdmissionPolicyBinding](t, objects[i+1])

		require.Equal(t, policy.Name, binding.Name)
		require.Equal(t, policy.Name, binding.Spec.PolicyName)
		require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
		require.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
		require.Len(t, policy.Spec.MatchConditions, 1)
		require.Equal(t, `request.userInfo.username == "system:serviceaccount:unbounded-system:racer-dataplane"`, policy.Spec.MatchConditions[0].Expression)
	}
}

func ptr[T any](v T) *T { return &v }
