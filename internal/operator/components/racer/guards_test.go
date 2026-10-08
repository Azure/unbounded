// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/operator/component"
)

func assertControllerBindings(t *testing.T, env *component.Env, present bool) {
	t.Helper()

	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: controllerName, Namespace: env.Namespace}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
	} {
		err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj)
		if present {
			require.NoError(t, err)
		} else {
			require.True(t, apierrors.IsNotFound(err), "%v", err)
		}
	}
}

func TestGuardContainmentRetainedInstallation(t *testing.T) {
	for _, name := range guardNames {
		for _, guard := range []client.Object{&admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicyBinding{}} {
			t.Run(fmt.Sprintf("%s/%T", name, guard), func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				initialize(t, env)

				before := &corev1.Secret{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), before))
				guard.SetName(name)
				require.NoError(t, env.Client.Delete(t.Context(), guard))
				require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
				plan := planPass(t, env)
				require.Len(t, plan.Operations, 2)
				persist(t, env, plan)
				assertControllerBindings(t, env, false)
				require.Zero(t, planPass(t, env).Len())

				for _, obj := range []client.Object{
					&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: claimName}},
					&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: markerName}},
					&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName}},
					&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tlsName}},
					&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
				} {
					require.NoError(t, env.Client.Get(t.Context(), objectKey(env, obj.GetName()), obj))
				}

				after := &corev1.Secret{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), after))
				require.Equal(t, before, after)
				require.NoError(t, env.Client.Create(t.Context(), cacheObject("later")))
				persist(t, env, planPass(t, env))
				assertControllerBindings(t, env, true)
			})
		}
	}
}

func TestGuardContainmentReaderFailures(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-only", true: "known-missing"}[missing], func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			if missing {
				require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: guardNames[0]}}))
			}

			env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == guardNames[1] {
						return errors.New("guard read unavailable")
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			if !missing {
				require.ErrorContains(t, err, "guard read unavailable")
				require.Nil(t, plan)
				assertControllerBindings(t, env, true)

				return
			}

			require.NoError(t, err)
			persist(t, env, plan)
			assertControllerBindings(t, env, false)
			plan, _, err = (Component{}).Plan(t.Context(), env, nil)
			require.ErrorContains(t, err, "guard read unavailable")
			require.Nil(t, plan)
		})
	}
}

func TestGuardContainmentRetriesFailedDeletion(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: guardNames[0]}}))
	original := env.Client
	env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == "RoleBinding" {
				return errors.New("delete unavailable")
			}

			return c.Delete(ctx, obj, opts...)
		},
	})
	result, err := env.Execute(t.Context(), planPass(t, env))
	require.NoError(t, err)
	require.ErrorContains(t, result.Err(), "delete unavailable")
	plan := planPass(t, env)
	require.Len(t, plan.Operations, 2)

	for _, op := range plan.Operations {
		require.Equal(t, component.OpDelete, op.Kind)
	}

	env.Client = original
	persist(t, env, plan)
	assertControllerBindings(t, env, false)
	persist(t, env, planPass(t, env))
	assertControllerBindings(t, env, true)
}

func TestGuardWatch(t *testing.T) {
	p := guardPredicate()

	for _, name := range append([]string{"unrelated"}, guardNames...) {
		obj := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
		want := name != "unrelated"
		require.Equal(t, want, p.Create(event.CreateEvent{Object: obj}))
		require.Equal(t, want, p.Delete(event.DeleteEvent{Object: obj}))
		require.Equal(t, want, p.Update(event.UpdateEvent{ObjectOld: obj, ObjectNew: obj.DeepCopy()}))
	}
}

func TestGuardContainmentTerminatingGuard(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	guard := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))
	guard.Finalizers = []string{"test.example/hold"}
	require.NoError(t, env.Client.Update(t.Context(), guard))
	require.NoError(t, env.Client.Delete(t.Context(), guard))
	persist(t, env, planPass(t, env))
	assertControllerBindings(t, env, false)
	plan, result, err := (Component{}).Plan(t.Context(), env, nil)
	require.NoError(t, err)
	require.Zero(t, plan.Len())
	require.Positive(t, result.RequeueAfter)
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))
	guard.Finalizers = nil
	require.NoError(t, env.Client.Update(t.Context(), guard))
	persist(t, env, planPass(t, env))
	assertControllerBindings(t, env, true)
}

func TestGuardContainmentBindingReadFailure(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: guardNames[0]}}))
	env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *rbacv1.RoleBinding, *rbacv1.ClusterRoleBinding:
				return errors.New("binding read unavailable")
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})
	persist(t, env, planPass(t, env))
	assertControllerBindings(t, env, false)

	plan := planPass(t, env)
	for _, op := range plan.Operations {
		require.Equal(t, component.OpDelete, op.Kind)
	}
}

func TestHealthyGuardsSurviveUnrelatedWriteFailure(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	before := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, before))
	require.NoError(t, env.Client.Delete(t.Context(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: controllerName, Namespace: env.Namespace}}))
	env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
			require.NoError(t, err)

			if data["kind"] == "Deployment" {
				return errors.New("workload unavailable")
			}

			return c.Apply(ctx, cfg, opts...)
		},
	})

	plan := planPass(t, env)
	for _, op := range plan.Operations {
		require.NotEqual(t, component.OpDelete, op.Kind)
	}

	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.ErrorContains(t, result.Err(), "workload unavailable")
	assertControllerBindings(t, env, true)

	after := &admissionv1.ValidatingAdmissionPolicy{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, after))
	require.Equal(t, before, after)
}

func TestGuardContainmentSpecTampering(t *testing.T) {
	cases := []struct {
		name   string
		object func() client.Object
		mutate func(client.Object)
	}{
		{"validation", func() client.Object { return &admissionv1.ValidatingAdmissionPolicy{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicy).Spec.Validations[0].Expression = "true"
		}},
		{"identity", func() client.Object { return &admissionv1.ValidatingAdmissionPolicy{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicy).Spec.MatchConditions[0].Expression = "false"
		}},
		{"failure-policy", func() client.Object { return &admissionv1.ValidatingAdmissionPolicy{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicy).Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
		}},
		{"resource-rules", func() client.Object { return &admissionv1.ValidatingAdmissionPolicy{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicy).Spec.MatchConstraints.ResourceRules[0].Resources = []string{"pods"}
		}},
		{"policy-name", func() client.Object { return &admissionv1.ValidatingAdmissionPolicyBinding{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicyBinding).Spec.PolicyName = "unrelated"
		}},
		{"validation-actions", func() client.Object { return &admissionv1.ValidatingAdmissionPolicyBinding{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicyBinding).Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
		}},
		{"binding-selector", func() client.Object { return &admissionv1.ValidatingAdmissionPolicyBinding{} }, func(obj client.Object) {
			obj.(*admissionv1.ValidatingAdmissionPolicyBinding).Spec.MatchResources = &admissionv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"bypass": "true"}},
			}
		}},
	}
	for _, retained := range []bool{false, true} {
		for _, name := range guardNames {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("retained=%t/%s/%s", retained, name, tc.name), func(t *testing.T) {
					env := testEnv(t, cacheObject("cache"))
					initialize(t, env)

					guard := tc.object()
					require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: name}, guard))
					original := guard.DeepCopyObject().(client.Object)
					tc.mutate(guard)
					require.NoError(t, env.Client.Update(t.Context(), guard))

					if retained {
						require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
					}

					plan := planPass(t, env)
					require.Len(t, plan.Operations, 2)

					for _, op := range plan.Operations {
						require.Equal(t, component.OpDelete, op.Kind)
					}

					require.Equal(t, plan.Summary(), planPass(t, env).Summary())
					persist(t, env, plan)
					assertControllerBindings(t, env, false)

					if retained {
						require.Zero(t, planPass(t, env).Len())
						require.NoError(t, env.Client.Create(t.Context(), cacheObject("later")))
					}

					persist(t, env, planPass(t, env))
					assertControllerBindings(t, env, false)
					persist(t, env, planPass(t, env))
					assertControllerBindings(t, env, true)
					require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: name}, guard))
					require.Equal(t, effectiveGuardSpec(original), effectiveGuardSpec(guard))
					_, stop, err := planGuardContainment(t.Context(), env)
					require.NoError(t, err)
					require.False(t, stop)
				})
			}
		}
	}
}

func TestGuardContainmentFailedSpecRepair(t *testing.T) {
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		t.Run(kind, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			var guard client.Object = &admissionv1.ValidatingAdmissionPolicyBinding{}
			if kind == "ValidatingAdmissionPolicy" {
				guard = &admissionv1.ValidatingAdmissionPolicy{}
			}

			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: guardNames[0]}, guard))

			switch guard := guard.(type) {
			case *admissionv1.ValidatingAdmissionPolicy:
				guard.Spec.Validations[0].Expression = "true"
			case *admissionv1.ValidatingAdmissionPolicyBinding:
				guard.Spec.PolicyName = "unrelated"
			}

			require.NoError(t, env.Client.Update(t.Context(), guard))
			persist(t, env, planPass(t, env))
			assertControllerBindings(t, env, false)
			original := env.Client
			env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == kind {
						return errors.New("guard repair unavailable")
					}

					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			result, err := env.Execute(t.Context(), planPass(t, env))
			require.NoError(t, err)
			require.ErrorContains(t, result.Err(), "guard repair unavailable")
			assertControllerBindings(t, env, false)
			env.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return nil
				},
			})
			persist(t, env, planPass(t, env))
			stillDamaged := planPass(t, env)
			require.Len(t, stillDamaged.Operations, 1)
			require.Equal(t, component.OpMergePatch, stillDamaged.Operations[0].Kind)
			assertControllerBindings(t, env, false)
			env.Client = original
			persist(t, env, planPass(t, env))
			assertControllerBindings(t, env, false)
			persist(t, env, planPass(t, env))
			assertControllerBindings(t, env, true)
		})
	}
}

func TestGuardContainmentManifestDefaults(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	for _, name := range guardNames {
		policy := &admissionv1.ValidatingAdmissionPolicy{}
		require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: name}, policy))
		require.Contains(t, policy.Spec.MatchConditions[0].Expression, ":custom-system:")
		require.NotContains(t, policy.Spec.MatchConditions[0].Expression, ":unbounded-system:")
		policy.Spec.FailurePolicy = nil
		policy.Spec.MatchConstraints.MatchPolicy = ptr.To(admissionv1.Equivalent)
		policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{}
		policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}

		for i := range policy.Spec.MatchConstraints.ResourceRules {
			policy.Spec.MatchConstraints.ResourceRules[i].Scope = ptr.To(admissionv1.AllScopes)
		}

		policy.Labels = map[string]string{"unrelated": "metadata"}
		require.NoError(t, env.Client.Update(t.Context(), policy))

		binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
		require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: name}, binding))
		binding.Spec.MatchResources = &admissionv1.MatchResources{
			MatchPolicy: ptr.To(admissionv1.Equivalent), NamespaceSelector: &metav1.LabelSelector{}, ObjectSelector: &metav1.LabelSelector{},
		}
		require.NoError(t, env.Client.Update(t.Context(), binding))
	}

	for _, retained := range []bool{false, true} {
		if retained {
			require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
		}

		plan, stop, err := planGuardContainment(t.Context(), env)
		require.NoError(t, err)
		require.False(t, stop)
		require.Nil(t, plan)
		assertControllerBindings(t, env, true)
	}
}
