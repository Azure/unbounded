// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	allocationRBACFile        = "05-machineallocation-rbac.yaml.tmpl"
	allocationRefPolicyFile   = "06-machine-allocationref-policy.yaml.tmpl"
	bootstrapTokenPolicyFile  = "07-machineallocation-bootstrap-token-policy.yaml.tmpl"
	bootstrapTokenPolicyName  = "unbounded-machineallocation-bootstrap-tokens"
	allocationRefPolicyName   = "unbounded-machine-allocationref"
	allocationRefPolicyDenial = "only an identity granted the allocate verb on machines may set spec.allocationRef"
	bootstrapTokenDenial      = "an identity granted the allocate verb on machines may create and delete only bootstrap token Secrets labeled unbounded-cloud.io/allocation-uid in kube-system"
)

func TestMachineAllocationRBACGrantsExactly(t *testing.T) {
	t.Parallel()

	want := map[string][]string{
		"unbounded-machineallocation-controller": {
			"unbounded-cloud.io/machineallocations:get",
			"unbounded-cloud.io/machineallocations:list",
			"unbounded-cloud.io/machineallocations:watch",
			"unbounded-cloud.io/machineallocations:patch",
			"unbounded-cloud.io/machineallocations/status:get",
			"unbounded-cloud.io/machineallocations/status:update",
			"unbounded-cloud.io/machineallocations/status:patch",
			"unbounded-cloud.io/machineclasses:get",
			"unbounded-cloud.io/machineclasses:list",
			"unbounded-cloud.io/machineclasses:watch",
			"unbounded-cloud.io/sites:get",
			"unbounded-cloud.io/sites:list",
			"unbounded-cloud.io/sites:watch",
			"unbounded-cloud.io/machineconfigurationversions:get",
			"unbounded-cloud.io/machineconfigurationversions:list",
			"unbounded-cloud.io/machineconfigurationversions:watch",
			"unbounded-cloud.io/machines:get",
			"unbounded-cloud.io/machines:list",
			"unbounded-cloud.io/machines:watch",
			"unbounded-cloud.io/machines:create",
			"unbounded-cloud.io/machines:patch",
			"unbounded-cloud.io/machines:allocate",
			"unbounded-cloud.io/machineoperations:get",
			"unbounded-cloud.io/machineoperations:list",
			"unbounded-cloud.io/machineoperations:watch",
			"unbounded-cloud.io/machineoperations:create",
			"unbounded-cloud.io/machineoperations:patch",
			"/nodes:get",
			"/nodes:list",
			"/nodes:watch",
		},
		"unbounded-machineallocation-controller-provider-config": {
			"/configmaps:get",
			"/configmaps:list",
			"/configmaps:watch",
			"/configmaps:patch",
		},
		"unbounded-machineallocation-controller-bootstrap-tokens": {
			"/secrets:get",
			"/secrets:create",
			"/secrets:delete",
		},
		"unbounded-machineallocation-operations": {
			"unbounded-cloud.io/machineallocations:get",
			"unbounded-cloud.io/sites:get",
		},
		"unbounded-machineallocation-operations-bootstrap-tokens": {
			"/secrets:get",
		},
	}

	got := map[string][]string{}

	for _, document := range manifestDocuments(t, allocationRBACFile) {
		var typeMeta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(document), &typeMeta))
		require.Equal(t, rbacv1.SchemeGroupVersion.String(), typeMeta.APIVersion, "the template must hold only ClusterRoles")
		require.Equal(t, "ClusterRole", typeMeta.Kind, "the template must hold only ClusterRoles")

		var clusterRole rbacv1.ClusterRole
		require.NoError(t, yaml.UnmarshalStrict([]byte(document), &clusterRole))
		require.NotContains(t, got, clusterRole.Name, "ClusterRole %s appears more than once", clusterRole.Name)
		require.Nil(t, clusterRole.AggregationRule, "ClusterRole %s must not aggregate other roles", clusterRole.Name)

		got[clusterRole.Name] = grantSet(t, clusterRole)
	}

	require.Equal(t, slices.Sorted(maps.Keys(want)), slices.Sorted(maps.Keys(got)))

	for name, grants := range want {
		slices.Sort(grants)
		require.Equal(t, grants, got[name], "ClusterRole %s", name)
	}
}

func TestMachineAllocationRefPolicyShape(t *testing.T) {
	t.Parallel()

	failurePolicy := admissionregistrationv1.Fail
	policy, binding := policyDocuments(t, allocationRefPolicyFile)

	require.Equal(t, admissionregistrationv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: &failurePolicy,
		MatchConstraints: &admissionregistrationv1.MatchResources{
			ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"unbounded-cloud.io"},
						APIVersions: []string{"*"},
						Resources:   []string{"machines"},
					},
				},
			}},
		},
		Validations: []admissionregistrationv1.Validation{{
			Expression: "!has(object.spec) || !has(object.spec.allocationRef) || " +
				"authorizer.group('unbounded-cloud.io').resource('machines').check('allocate').allowed()",
			Message: allocationRefPolicyDenial,
			Reason:  ptrTo(metav1.StatusReasonForbidden),
		}},
	}, policy.Spec)
	require.Equal(t, allocationRefPolicyName, policy.Name)
	requireDenyBinding(t, binding, allocationRefPolicyName)
}

func TestMachineAllocationBootstrapTokenPolicyShape(t *testing.T) {
	t.Parallel()

	failurePolicy := admissionregistrationv1.Fail
	policy, binding := policyDocuments(t, bootstrapTokenPolicyFile)

	require.Equal(t, bootstrapTokenPolicyName, policy.Name)
	require.Equal(t, &failurePolicy, policy.Spec.FailurePolicy)
	require.Equal(t, &admissionregistrationv1.MatchResources{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"secrets"},
				},
			},
		}},
	}, policy.Spec.MatchConstraints)
	require.Equal(t, []admissionregistrationv1.MatchCondition{{
		Name: "allocation-controller",
		Expression: "authorizer.group('unbounded-cloud.io').resource('machines').check('allocate').allowed() && " +
			"!authorizer.group('unbounded-cloud.io').resource('machines').check('*').allowed()",
	}}, policy.Spec.MatchConditions)
	require.Equal(t, []admissionregistrationv1.Validation{
		{
			Expression: "request.operation != 'CREATE' || (has(object.type) && object.type == 'bootstrap.kubernetes.io/token' && " +
				"has(object.metadata.labels) && 'unbounded-cloud.io/allocation-uid' in object.metadata.labels && " +
				"object.metadata.labels['unbounded-cloud.io/allocation-uid'] != '')",
			Message: bootstrapTokenDenial,
			Reason:  ptrTo(metav1.StatusReasonForbidden),
		},
		{
			Expression: "request.operation != 'DELETE' || (has(oldObject.type) && oldObject.type == 'bootstrap.kubernetes.io/token' && " +
				"has(oldObject.metadata.labels) && 'unbounded-cloud.io/allocation-uid' in oldObject.metadata.labels && " +
				"oldObject.metadata.labels['unbounded-cloud.io/allocation-uid'] != '')",
			Message: bootstrapTokenDenial,
			Reason:  ptrTo(metav1.StatusReasonForbidden),
		},
	}, policy.Spec.Validations)
	require.Nil(t, policy.Spec.ParamKind)
	require.Empty(t, policy.Spec.Variables)
	require.Empty(t, policy.Spec.AuditAnnotations)
	requireDenyBinding(t, binding, bootstrapTokenPolicyName)
}

// manifestDocuments returns the non-empty YAML documents of a template that
// must have no template directive.
func manifestDocuments(t *testing.T, name string) []string {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)
	require.NotContains(t, string(data), "{{", "%s contains a template directive", name)
	require.NotContains(t, string(data), "}}", "%s contains a template directive", name)

	var documents []string

	for _, document := range strings.Split(string(data), "\n---") {
		var typeMeta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(document), &typeMeta))

		if typeMeta.Kind != "" {
			documents = append(documents, document)
		}
	}

	return documents
}

// policyDocuments returns the one ValidatingAdmissionPolicy and the one
// binding a policy file must hold, in that order, and nothing else.
func policyDocuments(
	t *testing.T,
	name string,
) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()

	documents := manifestDocuments(t, name)
	require.Len(t, documents, 2, "%s must hold one policy and its binding", name)

	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.UnmarshalStrict([]byte(documents[0]), &policy))
	require.Equal(t, admissionregistrationv1.SchemeGroupVersion.String(), policy.APIVersion)
	require.Equal(t, "ValidatingAdmissionPolicy", policy.Kind)

	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.UnmarshalStrict([]byte(documents[1]), &binding))
	require.Equal(t, admissionregistrationv1.SchemeGroupVersion.String(), binding.APIVersion)
	require.Equal(t, "ValidatingAdmissionPolicyBinding", binding.Kind)

	return &policy, &binding
}

func requireDenyBinding(t *testing.T, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding, policyName string) {
	t.Helper()

	require.Equal(t, policyName, binding.Name)
	require.Equal(t, admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
		PolicyName:        policyName,
		ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
	}, binding.Spec)
	require.Nil(t, binding.Spec.MatchResources)
}

func ptrTo[T any](value T) *T {
	return &value
}
