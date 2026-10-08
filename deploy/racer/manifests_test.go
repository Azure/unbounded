// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestEmbeddedInventoryIsControllerOnly(t *testing.T) {
	files, err := fs.Glob(manifests.Manifests, "*")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"config.yaml", "controller.yaml", "controller-pdb.yaml", "create-restriction.yaml", "node-restriction.yaml", "rbac.yaml"}, files)

	for _, namespace := range []string{"unbounded-system", "custom-system"} {
		t.Run(namespace, func(t *testing.T) {
			env := &component.Env{Namespace: namespace}
			objects, err := env.DecodeManifestFS(manifests.Manifests, nil)
			require.NoError(t, err)

			inventory := []string{}
			for _, obj := range objects {
				inventory = append(inventory, obj.GetKind()+"/"+obj.GetName())
				if obj.GetNamespace() != "" {
					require.Equal(t, namespace, obj.GetNamespace())
				}

				require.NotContains(t, obj.GetName(), "dataplane")

				if obj.GetKind() == "ValidatingAdmissionPolicy" {
					policy := &admissionv1.ValidatingAdmissionPolicy{}
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, policy))
					require.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)
					require.Len(t, policy.Spec.MatchConditions, 1)
					require.Equal(t, `request.userInfo.username == "system:serviceaccount:`+namespace+`:racer-controller"`, policy.Spec.MatchConditions[0].Expression)
				}

				if obj.GetKind() == "ValidatingAdmissionPolicyBinding" {
					binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, binding))
					require.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, binding.Spec.ValidationActions)
					require.Equal(t, binding.Name, binding.Spec.PolicyName)
				}
			}

			require.ElementsMatch(t, []string{
				"ConfigMap/racer-config", "Deployment/racer-controller", "Service/racer-controller", "PodDisruptionBudget/racer-controller",
				"ServiceAccount/racer-controller", "Role/racer-controller", "RoleBinding/racer-controller", "ClusterRole/racer-controller", "ClusterRoleBinding/racer-controller",
				"ValidatingAdmissionPolicy/racer-runtime-write-restriction", "ValidatingAdmissionPolicyBinding/racer-runtime-write-restriction",
				"ValidatingAdmissionPolicy/racer-node-write-restriction", "ValidatingAdmissionPolicyBinding/racer-node-write-restriction",
			}, inventory)
		})
	}
}
