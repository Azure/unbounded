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

		if result.Reason != reasonInitializing {
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
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, "racer-controller"), &appsv1.Deployment{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, "racer-config"), &corev1.ConfigMap{})))

	container := ds.Spec.Template.Spec.Containers[0]
	require.Equal(t, "mirror.test/unbounded/racer:v1", container.Image)
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
		require.Contains(t, policy.Spec.MatchConditions[0].Expression, "system:serviceaccount:custom-system:racer")
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
	lease.Spec.HolderIdentity = ptr.To("racer-abc")
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
		"state":     &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: ns}},
		"lease":     &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "racer-seat-3", Namespace: ns}},
		"dataplane": &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"account":   &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"config":    &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: dataplaneConfigName, Namespace: ns}},
		"binding":   &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}},
		"webhook":   &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: webhookConfigName}},
		"endpoints": &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: webhookName, Namespace: ns}, AddressType: discoveryv1.AddressTypeIPv4},
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
			require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-lease"}}))
		},
		"deleted binding": func(t *testing.T, env *component.Env) {
			require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "racer-state"}}))
		},
		"tampered policy": func(t *testing.T, env *component.Env) {
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-pod-report"}, policy))
			policy.Spec.Validations = policy.Spec.Validations[:1]
			require.NoError(t, env.Client.Update(t.Context(), policy))
		},
		"audit only binding": func(t *testing.T, env *component.Env) {
			binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-webhook-configuration"}, binding))
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
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-lease"}}))

	plan := planPass(t, env)

	for _, op := range plan.Operations {
		require.False(t, op.Kind == component.OpDelete && op.Object.GetKind() == "ClusterRoleBinding")
	}
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
	config.Data["RACER_MAX_THREADS"] = "4"
	require.NoError(t, env.Client.Update(t.Context(), config))

	plan := planPass(t, env)
	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, "8", config.Data["RACER_SHARES"], "administrator edits survive")
	require.NotEqual(t, before, liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])

	for _, value := range []string{"2", "3", "x", "-1"} {
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

	// A present but empty value is not the default; only an absent key is.
	for _, value := range []string{"auto", "", "true", "CDI"} {
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
		config.Data[workload.NVIDIAKey] = value
		require.NoError(t, env.Client.Update(t.Context(), config))

		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorContains(t, err, workload.NVIDIAKey, "value %q", value)
	}
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
	require.True(t, p.Generic(event.GenericEvent{Object: &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-lease"}}}))
	require.False(t, p.Generic(event.GenericEvent{Object: &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "other"}}}))
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
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "racer-pod-report"}}))

	plan, result := planResult(t, env)
	require.Equal(t, reasonGuards, result.Reason)

	for _, op := range plan.Operations {
		require.Contains(t, []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "RoleBinding", "ClusterRoleBinding"}, op.Object.GetKind(), "no workload repair")
	}

	persist(t, env, plan)
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, dataplaneName), &rbacv1.RoleBinding{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKey{Name: dataplaneName}, &rbacv1.ClusterRoleBinding{})))
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: "racer-pod-report"}, &admissionv1.ValidatingAdmissionPolicy{}))

	// Retained mode never re-grants; a later cache restores the bindings.
	plan, result = planResult(t, env)
	require.Zero(t, plan.Len())
	require.True(t, result.Ready)
	liveDataplane(t, env)
}
