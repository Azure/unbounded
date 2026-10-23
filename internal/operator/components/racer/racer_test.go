// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, racerv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(uuid.NewString()))
			return c.Create(ctx, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			// Model persistence only; this fake does not enforce admission,
			// RBAC, or server-side apply field ownership.
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
			if err != nil {
				return err
			}

			obj := &unstructured.Unstructured{Object: data}
			current := obj.DeepCopy()

			err = c.Get(ctx, client.ObjectKeyFromObject(obj), current)
			if apierrors.IsNotFound(err) {
				obj.SetUID(types.UID(uuid.NewString()))
				return c.Create(ctx, obj)
			}

			if err != nil {
				return err
			}

			if obj.GetUID() != "" && obj.GetUID() != current.GetUID() {
				return apierrors.NewConflict(corev1.Resource(obj.GetKind()), obj.GetName(), errors.New("apply precondition failed"))
			}

			obj.SetResourceVersion(current.GetResourceVersion())
			obj.SetUID(current.GetUID())

			return c.Update(ctx, obj)
		},
	}).Build()

	return &component.Env{Client: c, APIReader: c, Scheme: scheme, Namespace: "custom-system", Config: component.Config{ImageRegistry: "mirror.test/unbounded", ImageTag: "v1"}}
}

func cacheObject(name string) *racerv1.ClusterCache {
	return &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uuid.NewString())}}
}

func planPass(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()
	plan, _ := planResult(t, env)

	return plan
}

func persist(t *testing.T, env *component.Env, plan *component.Plan) {
	t.Helper()
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)

	for _, op := range result.Results {
		require.Equal(t, component.OpSucceeded, op.Status, "%s: %v", op.Ref, op.Err)
	}
}

// initialize runs passes until the dataplane is installed.
func initialize(t *testing.T, env *component.Env) {
	t.Helper()

	for range 5 {
		plan, result := planResult(t, env)
		persist(t, env, plan)

		if result.Reason != reasonInitializing && result.Reason != reasonMigrating {
			return
		}
	}

	t.Fatal("installation did not converge")
}

func dataplaneOp(t *testing.T, plan *component.Plan) component.Operation {
	t.Helper()

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "DaemonSet" && op.Object.GetName() == dataplaneName {
			return op
		}
	}

	t.Fatal("dataplane DaemonSet not planned")

	return component.Operation{}
}

func liveDataplane(t *testing.T, env *component.Env) *appsv1.DaemonSet {
	t.Helper()

	ds := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))

	return ds
}

func managed() map[string]string { return map[string]string{managerAnnotation: component.FieldOwner} }

func TestNoClusterCacheIsInert(t *testing.T) {
	env := testEnv(t)
	plan, result := planResult(t, env)
	require.Zero(t, plan.Len())
	require.True(t, result.Ready)

	// Removing the last cache retains everything, including runtime state.
	env = testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))

	plan, result = planResult(t, env)
	require.Zero(t, plan.Len())
	require.True(t, result.Ready)
	liveDataplane(t, env)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, stateName), &corev1.Secret{}))
}

func TestInstallCreatesStateBeforeDataplane(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))

	plan, result := planResult(t, env)
	require.Equal(t, reasonInitializing, result.Reason)
	require.False(t, planHasObject(plan, "DaemonSet", dataplaneName))

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "Secret", "Lease", "ConfigMap":
			require.Equal(t, component.OpCreateIfAbsent, op.Kind, "%s", op.Ref())
		}
	}

	persist(t, env, plan)

	state := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, stateName), state))
	require.Equal(t, corev1.SecretTypeOpaque, state.Type)
	require.Empty(t, state.Data)
	require.Equal(t, component.FieldOwner, state.Labels[managerLabel])

	leases := &coordinationv1.LeaseList{}
	require.NoError(t, env.Client.List(t.Context(), leases, client.InNamespace(env.Namespace)))
	require.Len(t, leases.Items, 17)

	for _, lease := range leases.Items {
		require.True(t, lease.Name == "racer-primary" || strings.HasPrefix(lease.Name, "racer-seat-"), lease.Name)
		require.Equal(t, "true", lease.Labels["racer.unbounded-cloud.io/control"])
		require.Nil(t, lease.Spec.HolderIdentity)
	}

	initialize(t, env)

	ds := liveDataplane(t, env)
	require.Equal(t, string(state.UID), ds.Annotations[stateUIDAnnotation])

	account := &corev1.ServiceAccount{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), account))
	require.Equal(t, string(state.UID), account.Annotations[stateUIDAnnotation])
	require.False(t, *account.AutomountServiceAccountToken)

	slice := &discoveryv1.EndpointSlice{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, webhookName), slice))
	require.Equal(t, discoveryv1.AddressTypeIPv4, slice.AddressType)
	require.Empty(t, slice.Endpoints)
	require.Equal(t, webhookName, slice.Labels[discoveryv1.LabelServiceName])

	service := &corev1.Service{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, webhookName), service))
	require.Empty(t, service.Spec.Selector)

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookConfigName}, webhook))
	require.Empty(t, webhook.Webhooks[0].ClientConfig.CABundle)
	require.Equal(t, webhookName, webhook.Webhooks[0].ClientConfig.Service.Name)
	require.Equal(t, env.Namespace, webhook.Webhooks[0].ClientConfig.Service.Namespace)
	// The client-socket webhook differs only in its label and match condition.
	require.Len(t, webhook.Webhooks, 2)
	socketHook := webhook.Webhooks[1]
	require.Equal(t, "racer-client-socket.racer.unbounded-cloud.io", socketHook.Name)
	require.Equal(t, &metav1.LabelSelector{MatchLabels: map[string]string{"racer.unbounded-cloud.io/client-socket": "enabled"}}, socketHook.ObjectSelector)
	require.Len(t, socketHook.MatchConditions, 1)
	require.Contains(t, socketHook.MatchConditions[0].Expression, "racer.unbounded-cloud.io/client-socket-injected")
	require.Equal(t, webhook.Webhooks[0].ClientConfig, socketHook.ClientConfig)
	require.Equal(t, webhook.Webhooks[0].Rules, socketHook.Rules)
	require.Equal(t, webhook.Webhooks[0].NamespaceSelector, socketHook.NamespaceSelector)
	require.Equal(t, admissionv1.Fail, *socketHook.FailurePolicy)

	// No controller workload or authority material.
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, legacyController), &appsv1.Deployment{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, "racer-config"), &corev1.ConfigMap{})))

	container := ds.Spec.Template.Spec.Containers[0]
	require.Equal(t, "mirror.test/unbounded/racer-dataplane:v1", container.Image)
	require.Equal(t, []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfigName}}}}, container.EnvFrom)

	for _, e := range container.Env {
		if e.Name == "RACER_OBJECT_IMAGE" {
			require.Equal(t, "mirror.test/unbounded/racer-object:v1", e.Value)
		}

		// The dataplane namespace is retargeted.
		require.NotContains(t, e.Value, "unbounded-system")
	}

	policies := &admissionv1.ValidatingAdmissionPolicyList{}
	require.NoError(t, env.Client.List(t.Context(), policies))
	require.Len(t, policies.Items, 5)

	for _, policy := range policies.Items {
		require.Contains(t, policy.Spec.MatchConditions[0].Expression, "system:serviceaccount:custom-system:racer-dataplane")
	}
}

func TestBindingsAndDataplaneWaitForPolicies(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	plan := planPass(t, env)

	var guards, bindings []component.ObjectRef

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			guards = append(guards, op.Ref())
		}
	}

	require.Len(t, guards, 10)

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "RoleBinding", "ClusterRoleBinding":
			require.ElementsMatch(t, guards, op.DependsOn)
			require.True(t, op.RevokeOnDependencyFailure)

			bindings = append(bindings, op.Ref())
		}
	}

	require.Len(t, bindings, 2)
	require.Subset(t, dataplaneOp(t, plan).DependsOn, bindings)

	rendered, err := plan.ExecutionOrder()
	require.NoError(t, err)

	order := strings.Split(rendered, "\n")

	for _, binding := range bindings {
		for _, guard := range guards {
			require.Less(t, positionOf(t, order, guard), positionOf(t, order, binding))
		}
	}
}

func positionOf(t *testing.T, order []string, ref component.ObjectRef) int {
	t.Helper()

	for i, step := range order {
		if strings.HasSuffix(step, " "+ref.String()) {
			return i
		}
	}

	t.Fatalf("%s not in execution order %v", ref, order)

	return -1
}

func TestRuntimeOwnedStateIsNeverWritten(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	// Simulate the runtime initializing state, holding a seat, and
	// publishing webhook trust and endpoints.
	state := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, stateName), state))
	state.Annotations = map[string]string{"racer.unbounded-cloud.io/initialized": "1"}
	state.Data = map[string][]byte{"cluster": []byte(state.UID), "state": []byte("{}")}
	require.NoError(t, env.Client.Update(t.Context(), state))

	lease := &coordinationv1.Lease{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, "racer-seat-0"), lease))
	lease.Spec.HolderIdentity = ptr.To("racer-dataplane-abc")
	lease.Annotations = map[string]string{"racer.unbounded-cloud.io/endpoint": "https://10.0.0.1:8443"}
	require.NoError(t, env.Client.Update(t.Context(), lease))

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookConfigName}, webhook))
	webhook.Webhooks[0].ClientConfig.CABundle = []byte("runtime-ca")
	require.NoError(t, env.Client.Update(t.Context(), webhook))

	plan := planPass(t, env)

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "Secret", "Lease", "EndpointSlice":
			t.Fatalf("runtime-owned %s must not be written after creation", op.Ref())
		case "MutatingWebhookConfiguration":
			// The apply never declares caBundle, so it never takes the field
			// from the runtime's field manager.
			webhooks, _, err := unstructured.NestedSlice(op.Object.Object, "webhooks")
			require.NoError(t, err)

			_, found, err := unstructured.NestedFieldNoCopy(webhooks[0].(map[string]any), "clientConfig", "caBundle")
			require.NoError(t, err)
			require.False(t, found)
		}
	}
}

func TestMissingOrReplacedStateFailsClosed(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "replaced"}[replace], func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			state := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, stateName), state))
			require.NoError(t, env.Client.Delete(t.Context(), state))

			if replace {
				require.NoError(t, env.Client.Create(t.Context(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: env.Namespace, Labels: map[string]string{managerLabel: component.FieldOwner}},
					Type:       corev1.SecretTypeOpaque,
				}))
			}

			_, _, err := (Component{}).Plan(t.Context(), env, nil)
			require.ErrorContains(t, err, map[bool]string{false: "missing", true: "replaced"}[replace])

			if !replace {
				require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, stateName), &corev1.Secret{})))
			}

			// Deleting the dataplane workload and account starts a new identity.
			require.NoError(t, env.Client.Delete(t.Context(), liveDataplane(t, env)))
			require.NoError(t, env.Client.Delete(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}}))
			initialize(t, env)
		})
	}
}

func TestUnmanagedObjectsAreNotAdopted(t *testing.T) {
	ns := "custom-system"

	for name, obj := range map[string]client.Object{
		"state":        &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: ns}},
		"lease":        &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "racer-seat-3", Namespace: ns}},
		"dataplane":    &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"account":      &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"config":       &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dataplaneConfigName, Namespace: ns}},
		"binding":      &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"webhook":      &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName}},
		"endpoints":    &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: webhookName, Namespace: ns}, AddressType: discoveryv1.AddressTypeIPv4},
		"legacyDeploy": &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}},
	} {
		t.Run(name, func(t *testing.T) {
			obj.SetUID(types.UID(uuid.NewString()))
			env := testEnv(t, cacheObject("cache"), obj)

			var err error

			for range 5 {
				var plan *component.Plan

				plan, _, err = (Component{}).Plan(t.Context(), env, nil)
				if err != nil {
					break
				}

				persist(t, env, plan)
			}

			require.ErrorIs(t, err, errNotOwned)
		})
	}
}

func TestGuardContainment(t *testing.T) {
	for name, damage := range map[string]func(*testing.T, *component.Env){
		"deleted policy": func(t *testing.T, env *component.Env) {
			require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane-lease"}}))
		},
		"deleted binding": func(t *testing.T, env *component.Env) {
			require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane-state"}}))
		},
		"tampered policy": func(t *testing.T, env *component.Env) {
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-dataplane-pod-report"}, policy))
			policy.Spec.Validations = policy.Spec.Validations[:1]
			require.NoError(t, env.Client.Update(t.Context(), policy))
		},
		"audit only binding": func(t *testing.T, env *component.Env) {
			binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-dataplane-webhook-configuration"}, binding))
			binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
			require.NoError(t, env.Client.Update(t.Context(), binding))
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)
			damage(t, env)

			plan, result := planResult(t, env)
			require.Equal(t, reasonGuards, result.Reason)
			require.False(t, planHasObject(plan, "DaemonSet", dataplaneName), "containment must not touch workloads")

			deleted := map[string]bool{}

			for _, op := range plan.Operations {
				if op.Kind == component.OpDelete {
					deleted[op.Object.GetKind()] = true
				} else {
					require.Contains(t, []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"}, op.Object.GetKind())
				}
			}

			require.Equal(t, map[string]bool{"RoleBinding": true, "ClusterRoleBinding": true}, deleted)
			persist(t, env, plan)

			require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, dataplaneName), &rbacv1.RoleBinding{})))
			liveDataplane(t, env)

			// Policies are intact again, so the grants return.
			plan, result = planResult(t, env)
			require.NotEqual(t, reasonGuards, result.Reason)
			persist(t, env, plan)
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), &rbacv1.RoleBinding{}))
		})
	}
}

func TestContainmentLeavesUnmanagedBindings(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	binding := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: dataplaneName}, binding))
	binding.Annotations = nil
	require.NoError(t, env.Client.Update(t.Context(), binding))
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane-lease"}}))

	plan := planPass(t, env)

	for _, op := range plan.Operations {
		require.False(t, op.Kind == component.OpDelete && op.Object.GetKind() == "ClusterRoleBinding")
	}
}

func legacyObjects(ns string) []client.Object {
	stamped := func() map[string]string {
		return map[string]string{managerAnnotation: component.FieldOwner, legacyInstallationUID: "marker-uid"}
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uuid.NewString()), Annotations: stamped()}
	}
	cluster := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, UID: types.UID(uuid.NewString()), Annotations: stamped()}
	}

	return []client.Object{
		&appsv1.Deployment{ObjectMeta: meta(legacyController), Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": legacyController}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "racer-controller-1", Namespace: ns, Labels: map[string]string{"app": legacyController}}},
		&corev1.Service{ObjectMeta: meta(legacyController)},
		&policyv1.PodDisruptionBudget{ObjectMeta: meta(legacyController)},
		&rbacv1.RoleBinding{ObjectMeta: meta(legacyController)},
		&rbacv1.Role{ObjectMeta: meta(legacyController)},
		&rbacv1.ClusterRoleBinding{ObjectMeta: cluster(legacyController)},
		&rbacv1.ClusterRole{ObjectMeta: cluster(legacyController)},
		&corev1.ServiceAccount{ObjectMeta: meta(legacyController)},
		&corev1.Secret{ObjectMeta: meta("racer-controller-tls")},
		&corev1.ConfigMap{ObjectMeta: meta("racer-config")},
		&corev1.ConfigMap{ObjectMeta: meta("racer-bootstrap-trust")},
		&admissionv1.ValidatingAdmissionPolicy{ObjectMeta: cluster("racer-node-write-restriction")},
		&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: cluster("racer-node-write-restriction")},
		// The retired controller's leader Lease was never operator-managed.
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}},
		// Old dataplane wiring is replaced in place.
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns, UID: "old-dataplane", Annotations: map[string]string{managerAnnotation: component.FieldOwner, legacyInstallationUID: "marker-uid"}},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: dataplaneContainerName, Env: []corev1.EnvVar{{Name: "RACER_CLUSTER_ID", Value: "old"}, {Name: "RACER_CONTROL_ENDPOINT", Value: "https://racer-controller"}}}},
				Volumes:    []corev1.Volume{{Name: "bootstrap"}},
			}}},
		},
		&corev1.ServiceAccount{ObjectMeta: meta(dataplaneName)},
		&admissionv1.MutatingWebhookConfiguration{
			ObjectMeta: cluster(webhookConfigName),
			Webhooks: []admissionv1.MutatingWebhook{{Name: "racer-object-sidecar.racer.unbounded-cloud.io", ClientConfig: admissionv1.WebhookClientConfig{
				CABundle: []byte("old-ca"), Service: &admissionv1.ServiceReference{Name: legacyController, Namespace: ns},
			}}},
		},
	}
}

func legacyIdentity(ns string, bound bool) []client.Object {
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyClaim, Namespace: ns, UID: "claim-uid", Annotations: managed()}, Data: map[string]string{"cluster": "c1", legacyMarkerUIDKey: "marker-uid"}}
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyMarker, Namespace: ns, UID: "marker-uid", Annotations: map[string]string{managerAnnotation: component.FieldOwner, legacyClaimUID: "claim-uid"}}, Data: map[string]string{"cluster": "c1"}}
	versionBinding := "marker-uid"

	if !bound {
		versionBinding = "another-marker"
	}

	version := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyVersion, Namespace: ns, UID: "version-uid", Annotations: map[string]string{legacyBoundUID: versionBinding, legacyCredentialID: "credentials-uid"}}}
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: legacyCredentials, Namespace: ns, UID: "credentials-uid"}}

	return []client.Object{claim, marker, version, credentials}
}

func exists(t *testing.T, env *component.Env, obj client.Object) bool {
	t.Helper()

	err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return false
	}

	require.NoError(t, err)

	return true
}

func TestLegacyControllerMigration(t *testing.T) {
	ns := "custom-system"
	objects := append(legacyObjects(ns), legacyIdentity(ns, true)...)
	// Unmanaged look-alikes survive.
	objects = append(objects, cacheObject("cache"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-bootstrap-trust-copy", Namespace: ns}})
	env := testEnv(t, objects...)

	// Step 1: stop the controller and nothing else.
	plan, result := planResult(t, env)
	require.Equal(t, reasonMigrating, result.Reason)
	require.Len(t, plan.Operations, 1)
	require.Equal(t, component.OpDelete, plan.Operations[0].Kind)
	require.Equal(t, "Deployment", plan.Operations[0].Object.GetKind())
	require.NotEmpty(t, plan.Operations[0].Object.GetUID(), "deletes carry a UID precondition")
	persist(t, env, plan)

	// Step 2: wait while a controller Pod is still terminating.
	plan, result = planResult(t, env)
	require.Equal(t, reasonMigrating, result.Reason)
	require.Zero(t, plan.Len())
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "racer-controller-1", Namespace: ns}}))

	// Step 3: remove managed controller objects and install.
	initialize(t, env)
	settle(t, env)

	for _, obj := range legacyObjects(ns)[:14] {
		require.False(t, exists(t, env, obj), "%T %s survived", obj, obj.GetName())
	}

	for _, obj := range legacyIdentity(ns, true) {
		require.False(t, exists(t, env, obj), "%T %s survived", obj, obj.GetName())
	}

	require.True(t, exists(t, env, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}))
	require.True(t, exists(t, env, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-bootstrap-trust-copy", Namespace: ns}}))

	ds := liveDataplane(t, env)
	require.NotContains(t, ds.Annotations, "unbounded-cloud.io/racer-installation-uid")

	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		require.NotContains(t, []string{"RACER_CLUSTER_ID", "RACER_CONTROL_ENDPOINT", "RACER_TRUST_BUNDLE"}, e.Name)
	}

	for _, v := range ds.Spec.Template.Spec.Volumes {
		require.NotEqual(t, "bootstrap", v.Name)
	}

	state := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, stateName), state))
	require.Equal(t, string(state.UID), ds.Annotations[stateUIDAnnotation])

	webhook := &admissionv1.MutatingWebhookConfiguration{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: webhookConfigName}, webhook))
	require.Equal(t, webhookName, webhook.Webhooks[0].ClientConfig.Service.Name)
}

func TestLegacyMigrationKeepsUnboundAndUnmanagedObjects(t *testing.T) {
	ns := "custom-system"
	unmanagedService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}
	env := testEnv(t, append(legacyIdentity(ns, false), cacheObject("cache"), unmanagedService)...)
	initialize(t, env)
	settle(t, env)

	identity := legacyIdentity(ns, false)
	require.False(t, exists(t, env, identity[0]), "managed claim is removed")
	require.False(t, exists(t, env, identity[1]), "managed marker is removed")
	require.True(t, exists(t, env, identity[2]), "version not bound to the marker is kept")
	require.True(t, exists(t, env, identity[3]), "credentials without a bound version are kept")
	require.True(t, exists(t, env, unmanagedService))
}

func TestDataplaneConfig(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	before := liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash]
	require.NotEmpty(t, before)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, component.FieldOwner, config.Annotations[managerAnnotation])
	require.NotContains(t, config.Data, "RACER_MAX_THREADS")

	config.Data["RACER_SHARES"] = "8"
	config.Data["RACER_MAX_THREADS"] = "3"
	require.NoError(t, env.Client.Update(t.Context(), config))

	plan := planPass(t, env)
	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, "8", config.Data["RACER_SHARES"], "administrator edits survive")
	require.NotEqual(t, before, liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])

	for _, value := range []string{"2", "x", "-1"} {
		config.Data["RACER_MAX_THREADS"] = value
		require.NoError(t, env.Client.Update(t.Context(), config))

		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorContains(t, err, "RACER_MAX_THREADS")
	}
}

func TestDataplaneNVIDIAConfig(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	// The default asks the NVIDIA runtime for driver libraries through env only.
	ds := liveDataplane(t, env)
	values := map[string]string{}

	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		values[e.Name] = e.Value
	}

	require.Equal(t, "all", values["NVIDIA_VISIBLE_DEVICES"])
	require.Equal(t, "compute,utility", values["NVIDIA_DRIVER_CAPABILITIES"])
	require.NotContains(t, ds.Spec.Template.Annotations, workload.CDIAnnotation)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	config.Data[workload.NVIDIAKey] = "cdi"
	require.NoError(t, env.Client.Update(t.Context(), config))
	persist(t, env, planPass(t, env))
	require.Equal(t, workload.CDIAllGPUs, liveDataplane(t, env).Spec.Template.Annotations[workload.CDIAnnotation])

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	config.Data[workload.NVIDIAKey] = "auto"
	require.NoError(t, env.Client.Update(t.Context(), config))

	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, workload.NVIDIAKey)
}

func TestValidateDataplaneOverride(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	original := dataplaneOp(t, planPass(t, env)).Object

	for name, tc := range map[string]struct {
		mutate func(map[string]any, map[string]any)
		ok     bool
	}{
		"resources":     {mutate: func(_, c map[string]any) { c["resources"] = map[string]any{"limits": map[string]any{"memory": "1Gi"}} }, ok: true},
		"node selector": {mutate: func(p, _ map[string]any) { p["nodeSelector"] = map[string]any{"pool": "cache"} }, ok: true},
		"host network":  {mutate: func(p, _ map[string]any) { p["hostNetwork"] = true }, ok: true},
		"extra env": {mutate: func(_, c map[string]any) {
			c["env"] = append(c["env"].([]any), map[string]any{"name": "RUST_LOG", "value": "debug"})
		}, ok: true},
		"owned env": {mutate: func(_, c map[string]any) {
			c["env"].([]any)[0] = map[string]any{"name": "RACER_NAMESPACE", "value": "other"}
		}},
		"envFrom":           {mutate: func(_, c map[string]any) { c["envFrom"] = []any{} }},
		"volume mounts":     {mutate: func(_, c map[string]any) { c["volumeMounts"] = []any{} }},
		"volumes":           {mutate: func(p, _ map[string]any) { p["volumes"] = []any{} }},
		"service account":   {mutate: func(p, _ map[string]any) { p["serviceAccountName"] = "default" }},
		"automount":         {mutate: func(p, _ map[string]any) { p["automountServiceAccountToken"] = true }},
		"malformed env":     {mutate: func(_, c map[string]any) { c["env"] = "x" }},
		"missing container": {mutate: func(_, c map[string]any) { c["name"] = "other" }},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := original.DeepCopy()
			pod, _, err := unstructured.NestedMap(candidate.Object, "spec", "template", "spec")
			require.NoError(t, err)

			container := pod["containers"].([]any)[0].(map[string]any)
			tc.mutate(pod, container)
			require.NoError(t, unstructured.SetNestedMap(candidate.Object, pod, "spec", "template", "spec"))

			err = ValidateDataplaneOverride(original, candidate)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestClusterCachePredicate(t *testing.T) {
	p := clusterCachePredicate()
	old := cacheObject("cache")
	deleting := old.DeepCopy()
	deleting.DeletionTimestamp = ptr.To(metav1.Now())

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: deleting}))
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: deleting, ObjectNew: deleting}))
}

func TestGuardPredicate(t *testing.T) {
	p := guardPredicate()
	require.True(t, p.Generic(event.GenericEvent{Object: &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane-lease"}}}))
	require.False(t, p.Generic(event.GenericEvent{Object: &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "other"}}}))
}

func TestLegacyMigrationRequiresInstallationStamp(t *testing.T) {
	ns := "custom-system"
	managedOnly := func(obj client.Object) client.Object {
		obj.SetAnnotations(managed())
		obj.SetUID(types.UID(uuid.NewString()))

		return obj
	}

	t.Run("deployment", func(t *testing.T) {
		env := testEnv(t, cacheObject("cache"), managedOnly(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}))
		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorIs(t, err, errNotOwned)
		require.True(t, exists(t, env, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}))
	})

	t.Run("objects", func(t *testing.T) {
		role := managedOnly(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: legacyController}})
		config := managedOnly(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyConfig, Namespace: ns}})
		env := testEnv(t, cacheObject("cache"), role, config)
		initialize(t, env)
		require.True(t, exists(t, env, role))
		require.True(t, exists(t, env, config))
	})
}

func TestLegacyMigrationRefusesDroppedWorkloadSettings(t *testing.T) {
	ns := "custom-system"

	for name, tc := range map[string]struct {
		data map[string]string
		want string
	}{
		"peer port":       {data: map[string]string{"RACER_PEER_PORT": "9000"}, want: "RACER_PEER_PORT"},
		"diagnostics":     {data: map[string]string{"RACER_DIAGNOSTICS_PORT": "9191"}, want: "RACER_DIAGNOSTICS_PORT"},
		"host network":    {data: map[string]string{"RACER_HOST_NETWORK": "true"}, want: "RACER_HOST_NETWORK"},
		"bad bool":        {data: map[string]string{"RACER_HOST_NETWORK": "yes"}, want: "RACER_HOST_NETWORK"},
		"pod network":     {data: map[string]string{"RACER_POD_NETWORK_NODES": "a"}, want: "RACER_POD_NETWORK_NODES"},
		"nvidia":          {data: map[string]string{"RACER_DATAPLANE_NVIDIA": "cdi"}, want: "RACER_DATAPLANE_NVIDIA"},
		"defaults accept": {data: map[string]string{"RACER_PEER_PORT": "8082", "RACER_HOST_NETWORK": "false", "RACER_DATAPLANE_NVIDIA": "env"}},
	} {
		t.Run(name, func(t *testing.T) {
			objects := legacyObjects(ns)
			for _, obj := range objects {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == legacyConfig {
					cm.Data = tc.data
				}
			}

			env := testEnv(t, append(append(objects, legacyIdentity(ns, true)...), cacheObject("cache"))...)
			plan, _, err := (Component{}).Plan(t.Context(), env, nil)

			if tc.want == "" {
				require.NoError(t, err)
				require.Equal(t, "Deployment", plan.Operations[0].Object.GetKind())

				return
			}

			require.ErrorContains(t, err, tc.want)
			require.True(t, exists(t, env, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}), "controller keeps running")
		})
	}
}

func TestTerminatingDataplaneStillRecordsState(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	ds := liveDataplane(t, env)
	ds.Finalizers = []string{"test/hold"}
	require.NoError(t, env.Client.Update(t.Context(), ds))
	require.NoError(t, env.Client.Delete(t.Context(), ds))
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}}))
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: env.Namespace}}))

	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "missing")
}

func TestControlSeatsConfig(t *testing.T) {
	for value, ok := range map[string]bool{"auto": true, "1": true, "2": true, "16": true, "0": false, "17": false, "-1": false, "Auto": false, "": false, "01": false, "+1": false, " 1": false, "1 ": false, "\t2": false} {
		t.Run("value "+value, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"), &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: dataplaneConfigName, Namespace: "custom-system", UID: "cfg", Annotations: managed()},
				Data:       map[string]string{"RACER_CONTROL_SEATS": value},
			})

			if !ok {
				_, _, err := (Component{}).Plan(t.Context(), env, nil)
				require.ErrorContains(t, err, "RACER_CONTROL_SEATS")

				return
			}

			initialize(t, env)

			if value != "2" {
				return
			}

			// The ConfigMap value reaches the Pod: no explicit env shadows it.
			container := liveDataplane(t, env).Spec.Template.Spec.Containers[0]
			for _, e := range container.Env {
				require.NotEqual(t, "RACER_CONTROL_SEATS", e.Name)
			}

			require.Equal(t, dataplaneConfigName, container.EnvFrom[0].ConfigMapRef.Name)
		})
	}

	t.Run("seeded default", func(t *testing.T) {
		env := testEnv(t, cacheObject("cache"))
		initialize(t, env)

		cm := &corev1.ConfigMap{}
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), cm))
		require.Equal(t, "auto", cm.Data["RACER_CONTROL_SEATS"])
	})
}

func TestOverrideCannotShadowOrDuplicateEnv(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	original := dataplaneOp(t, planAfterInit(t, env)).Object

	withEnv := func(extra ...any) *unstructured.Unstructured {
		candidate := original.DeepCopy()
		containers, _, _ := unstructured.NestedSlice(candidate.Object, "spec", "template", "spec", "containers")
		c := containers[0].(map[string]any)
		envs, _ := c["env"].([]any)
		c["env"] = append(append([]any{}, envs...), extra...)

		require.NoError(t, unstructured.SetNestedSlice(candidate.Object, containers, "spec", "template", "spec", "containers"))

		return candidate
	}

	require.NoError(t, ValidateDataplaneOverride(original, withEnv(map[string]any{"name": "RACER_EXTRA", "value": "1"})))
	require.ErrorContains(t, ValidateDataplaneOverride(original, withEnv(map[string]any{"name": "RACER_CONTROL_SEATS", "value": "16"})), "shadow")
	require.ErrorContains(t, ValidateDataplaneOverride(original, withEnv(map[string]any{"name": "RACER_PEER_LISTEN", "value": "[::]:1"})), "more than once")
	require.ErrorContains(t, ValidateDataplaneOverride(original, withEnv(map[string]any{"name": "RACER_EXTRA", "value": "1"}, map[string]any{"name": "RACER_EXTRA", "value": "2"})), "more than once")
}

func planAfterInit(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()
	initialize(t, env)

	return planPass(t, env)
}

func TestRetainedInstallStillContainsGuards(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane-pod-report"}}))

	plan, result := planResult(t, env)
	require.Equal(t, reasonGuards, result.Reason)

	for _, op := range plan.Operations {
		require.Contains(t, []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "RoleBinding", "ClusterRoleBinding"}, op.Object.GetKind(), "no workload repair")
	}

	persist(t, env, plan)
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, dataplaneName), &rbacv1.RoleBinding{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKey{Name: dataplaneName}, &rbacv1.ClusterRoleBinding{})))
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-dataplane-pod-report"}, &admissionv1.ValidatingAdmissionPolicy{}))

	// Retained mode never re-grants; a later cache restores the bindings.
	plan, result = planResult(t, env)
	require.Zero(t, plan.Len())
	require.True(t, result.Ready)
	liveDataplane(t, env)
}

// settle runs passes until a plan holds no delete or patch, failing when it
// does not.
func settle(t *testing.T, env *component.Env) {
	t.Helper()

	for range 12 {
		plan := planPass(t, env)

		pending := false

		for _, op := range plan.Operations {
			pending = pending || op.Kind == component.OpDelete || op.Kind == component.OpMergePatch
		}

		persist(t, env, plan)

		if !pending {
			return
		}
	}

	t.Fatal("plan did not settle")
}

func stampedWith(obj client.Object, stamp string) client.Object {
	obj.SetAnnotations(map[string]string{managerAnnotation: component.FieldOwner, legacyInstallationUID: stamp})
	obj.SetUID(types.UID(uuid.NewString()))

	return obj
}

func TestLegacyForeignStampIsNeverDeletedOrAdopted(t *testing.T) {
	ns := "custom-system"

	t.Run("deployment", func(t *testing.T) {
		deploy := stampedWith(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}, "other-marker")
		env := testEnv(t, append(legacyIdentity(ns, true), cacheObject("cache"), deploy)...)
		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorIs(t, err, errNotOwned)
		require.True(t, exists(t, env, deploy))
	})

	t.Run("static objects", func(t *testing.T) {
		role := stampedWith(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: legacyController}}, "other-marker")
		svc := stampedWith(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}, "other-marker")
		env := testEnv(t, append(legacyIdentity(ns, true), cacheObject("cache"), role, svc)...)
		initialize(t, env)
		settle(t, env)
		require.True(t, exists(t, env, role))
		require.True(t, exists(t, env, svc))
	})

	for name, obj := range map[string]client.Object{
		"dataplane": &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"account":   &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"webhook":   &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName}},
		"config":    &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dataplaneConfigName, Namespace: ns}},
	} {
		t.Run("adopt "+name, func(t *testing.T) {
			env := testEnv(t, append(legacyIdentity(ns, true), cacheObject("cache"), stampedWith(obj, "other-marker"))...)

			var err error

			for range 6 {
				var plan *component.Plan

				plan, _, err = (Component{}).Plan(t.Context(), env, nil)
				if err != nil {
					break
				}

				persist(t, env, plan)
			}

			require.ErrorIs(t, err, errNotOwned)
		})
	}
}

func TestLegacyAnchorsMissingOrInconsistentFailClosed(t *testing.T) {
	ns := "custom-system"

	for name, mutate := range map[string]func([]client.Object) []client.Object{
		"no anchors":      func(o []client.Object) []client.Object { return nil },
		"marker missing":  func(o []client.Object) []client.Object { return o[:1] },
		"claim missing":   func(o []client.Object) []client.Object { return o[1:2] },
		"replaced marker": func(o []client.Object) []client.Object { o[1].SetUID("marker-2"); return o },
		"claim rebound": func(o []client.Object) []client.Object {
			o[1].SetAnnotations(map[string]string{managerAnnotation: component.FieldOwner, legacyClaimUID: "x"})
			return o
		},
		"cluster mismatch": func(o []client.Object) []client.Object { o[1].(*corev1.ConfigMap).Data["cluster"] = "c2"; return o },
	} {
		t.Run(name, func(t *testing.T) {
			legacyObjs := legacyObjects(ns)
			env := testEnv(t, append(append(legacyObjs, mutate(legacyIdentity(ns, true))...), cacheObject("cache"))...)

			_, _, err := (Component{}).Plan(t.Context(), env, nil)
			require.ErrorIs(t, err, errNotOwned)

			// Once the controller is gone by hand, unproven objects stay.
			require.NoError(t, env.Client.Delete(t.Context(), legacyObjs[0]))
			require.NoError(t, env.Client.Delete(t.Context(), legacyObjs[1]))

			for range 4 {
				plan, _, err := (Component{}).Plan(t.Context(), env, nil)
				if err != nil {
					break
				}

				for _, op := range plan.Operations {
					require.NotEqual(t, component.OpDelete, op.Kind, "deleted %s %s", op.Object.GetKind(), op.Object.GetName())
				}

				persist(t, env, plan)
			}

			for _, obj := range legacyObjects(ns)[2:14] {
				require.True(t, exists(t, env, obj), "%T %s removed", obj, obj.GetName())
			}
		})
	}
}

func TestLegacyCleanupIsStagedAndResumable(t *testing.T) {
	ns := "custom-system"
	objects := append(legacyObjects(ns), legacyIdentity(ns, true)...)
	objects = append(objects, cacheObject("cache"))
	env := testEnv(t, objects...)
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "racer-controller-1", Namespace: ns}}))

	var failed bool

	for pass := 0; pass < 15; pass++ {
		plan, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.NoError(t, err)

		// Anchors are never deleted while a dependent object remains.
		for _, op := range plan.Operations {
			if op.Kind != component.OpDelete {
				continue
			}

			switch op.Object.GetName() {
			case legacyMarker:
				require.False(t, exists(t, env, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyVersion, Namespace: ns}}))
				require.False(t, exists(t, env, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: legacyCredentials, Namespace: ns}}))
				require.False(t, exists(t, env, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: legacyController, Namespace: ns}}))
				require.NotContains(t, liveDataplane(t, env).Annotations, legacyInstallationUID)
			case legacyClaim:
				require.False(t, exists(t, env, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyMarker, Namespace: ns}}))
			case legacyVersion:
				require.False(t, exists(t, env, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: legacyCredentials, Namespace: ns}}))
			}
		}

		// Simulate a crash: drop every other pass's writes once.
		if !failed && pass == 1 {
			failed = true
			continue
		}

		_, err = env.Execute(t.Context(), plan)
		require.NoError(t, err)
	}

	for _, obj := range legacyIdentity(ns, true) {
		require.False(t, exists(t, env, obj), "%T %s survived", obj, obj.GetName())
	}
}

// migratedEnv returns an env whose controller is stopped and whose legacy
// identity is built by mutate.
func migratedEnv(t *testing.T, mutate func(marker, version *corev1.ConfigMap)) *component.Env {
	t.Helper()

	ns := "custom-system"
	identity := legacyIdentity(ns, true)
	mutate(identity[1].(*corev1.ConfigMap), identity[2].(*corev1.ConfigMap))

	objects := append(legacyObjects(ns), identity...)
	env := testEnv(t, append(objects, cacheObject("cache"))...)
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "racer-controller-1", Namespace: ns}}))

	return env
}

func runPasses(t *testing.T, env *component.Env, passes int) error {
	t.Helper()

	for range passes {
		plan, _, err := (Component{}).Plan(t.Context(), env, nil)
		if err != nil {
			return err
		}

		_, err = env.Execute(t.Context(), plan)
		require.NoError(t, err)
	}

	return nil
}

func TestLegacyTerminatingVersionHoldsMarker(t *testing.T) {
	for name, recorded := range map[string]string{"pending marker": "", "consumed marker": "version-uid"} {
		t.Run(name, func(t *testing.T) {
			env := migratedEnv(t, func(marker, version *corev1.ConfigMap) {
				if recorded != "" {
					marker.Data[legacyVersionUIDKey] = recorded
				}

				version.Finalizers = []string{"test/hold"}
			})
			ns := env.Namespace
			version := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyVersion, Namespace: ns}}
			marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyMarker, Namespace: ns}}

			require.NoError(t, runPasses(t, env, 12))
			require.True(t, exists(t, env, version))
			require.NotNil(t, version.DeletionTimestamp, "bound version is being deleted")
			require.True(t, exists(t, env, marker), "marker kept while the version terminates")
			require.True(t, exists(t, env, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: legacyClaim, Namespace: ns}}))

			// The finalizer completes later; cleanup resumes.
			version.Finalizers = nil
			require.NoError(t, env.Client.Update(t.Context(), version))
			require.NoError(t, runPasses(t, env, 4))

			for _, obj := range legacyIdentity(ns, true) {
				require.False(t, exists(t, env, obj), "%T %s survived", obj, obj.GetName())
			}
		})
	}
}

func TestLegacyReplacedVersionIsPreserved(t *testing.T) {
	for name, mutate := range map[string]func(marker, version *corev1.ConfigMap){
		"replaced uid": func(marker, version *corev1.ConfigMap) { marker.Data[legacyVersionUIDKey] = "version-old" },
		"rebound to other": func(marker, version *corev1.ConfigMap) {
			marker.Data[legacyVersionUIDKey] = "version-uid"
			version.Annotations[legacyBoundUID] = "x"
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := migratedEnv(t, mutate)
			ns := env.Namespace

			err := runPasses(t, env, 12)
			require.ErrorContains(t, err, legacyVersion)

			for _, obj := range legacyIdentity(ns, true) {
				require.True(t, exists(t, env, obj), "%T %s removed", obj, obj.GetName())
				require.Nil(t, obj.GetDeletionTimestamp())
			}
		})
	}
}
