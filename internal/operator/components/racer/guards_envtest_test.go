// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestEnvtestGuardContainmentDefaults(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API guard defaulting tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	admin, err := client.New(rc, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)

	env := &component.Env{Client: admin, APIReader: admin, Scheme: scheme.Scheme, Namespace: "custom-system"}
	ctx := t.Context()
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))

	claim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: env.Namespace, Annotations: map[string]string{managerAnnotation: component.FieldOwner}},
		Data:       map[string]string{"cluster": uuid.NewString(), "state": "consumed"},
		Immutable:  ptr.To(true),
	}
	require.NoError(t, admin.Create(ctx, claim))
	marker := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: env.Namespace, Annotations: map[string]string{managerAnnotation: component.FieldOwner, claimAnnotation: string(claim.UID)}},
		Data:       map[string]string{"cluster": claim.Data["cluster"], "version_configmap": versionName},
	}
	require.NoError(t, admin.Create(ctx, marker))

	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml", "node-restriction.yaml", "rbac.yaml"}, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		bindRuntime(obj, marker.UID)
		require.NoError(t, admin.Create(ctx, obj.DeepCopy()))
	}

	assertHealthy := func(t *testing.T) {
		t.Helper()

		for range 2 {
			plan, stop, err := planGuardContainment(t.Context(), env)
			require.NoError(t, err)
			require.False(t, stop)
			require.Nil(t, plan)
			assertControllerBindings(t, env, true)
		}
	}

	t.Run("persisted-defaults", func(t *testing.T) {
		for _, name := range guardNames {
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: name}, policy))
			require.Equal(t, ptr.To(admissionv1.Fail), policy.Spec.FailurePolicy)
			require.Equal(t, ptr.To(admissionv1.Equivalent), policy.Spec.MatchConstraints.MatchPolicy)
			require.Equal(t, &metav1.LabelSelector{}, policy.Spec.MatchConstraints.NamespaceSelector)
			require.Equal(t, &metav1.LabelSelector{}, policy.Spec.MatchConstraints.ObjectSelector)

			for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
				require.Equal(t, ptr.To(admissionv1.AllScopes), rule.Scope)
			}

			require.Contains(t, policy.Spec.MatchConditions[0].Expression, ":custom-system:")
		}

		assertHealthy(t)
	})

	for _, name := range guardNames {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			t.Run(name+"/"+kind+"-drift", func(t *testing.T) {
				var guard client.Object = &admissionv1.ValidatingAdmissionPolicyBinding{}
				if kind == "ValidatingAdmissionPolicy" {
					guard = &admissionv1.ValidatingAdmissionPolicy{}
				}

				require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: name}, guard))

				original := guard.DeepCopyObject().(client.Object)
				switch guard := guard.(type) {
				case *admissionv1.ValidatingAdmissionPolicy:
					guard.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
				case *admissionv1.ValidatingAdmissionPolicyBinding:
					guard.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
				}

				require.NoError(t, admin.Update(ctx, guard))

				plan, stop, err := planGuardContainment(t.Context(), env)
				require.NoError(t, err)
				require.True(t, stop)
				require.NotNil(t, plan)
				require.Len(t, plan.Operations, 2)

				for _, op := range plan.Operations {
					require.Equal(t, component.OpDelete, op.Kind)
					require.Contains(t, []string{"RoleBinding", "ClusterRoleBinding"}, op.Object.GetKind())
					require.Equal(t, controllerName, op.Object.GetName())
					require.NotEmpty(t, op.Object.GetUID())
					require.NotEmpty(t, op.Object.GetResourceVersion())
				}

				persist(t, env, plan)
				assertControllerBindings(t, env, false)

				// Containment revokes grants before changing a drifted guard.
				current := guard.DeepCopyObject().(client.Object)
				require.NoError(t, admin.Get(ctx, client.ObjectKeyFromObject(guard), current))
				require.Equal(t, effectiveGuardSpec(guard), effectiveGuardSpec(current))
				original.SetResourceVersion(current.GetResourceVersion())
				require.NoError(t, admin.Update(ctx, original))

				for _, obj := range objects {
					if obj.GetKind() == "RoleBinding" || obj.GetKind() == "ClusterRoleBinding" {
						require.NoError(t, admin.Create(ctx, obj.DeepCopy()))
					}
				}

				assertHealthy(t)
			})
		}
	}
}
