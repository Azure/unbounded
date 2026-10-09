// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	racercore "github.com/Azure/unbounded/internal/racer"
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
			// Model persistence only; this fake does not enforce admission or RBAC.
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
			if err != nil {
				return err
			}

			obj := &unstructured.Unstructured{Object: data}
			current := obj.DeepCopy()

			err = c.Get(ctx, client.ObjectKeyFromObject(obj), current)
			if apierrors.IsNotFound(err) {
				return c.Create(ctx, obj)
			}

			if err != nil {
				return err
			}

			if (obj.GetUID() != "" && obj.GetUID() != current.GetUID()) || (obj.GetResourceVersion() != "" && obj.GetResourceVersion() != current.GetResourceVersion()) {
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
	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.NoError(t, err)

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

func initialize(t *testing.T, env *component.Env) racercore.Config {
	t.Helper()

	for range 9 {
		plan := planPass(t, env)
		require.NotContains(t, plan.Summary(), "DaemonSet/")
		persist(t, env, plan)
	}

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

	cfg, err := racercore.ConfigFromLookup(func(key string) (string, bool) {
		if key == "POD_NAMESPACE" {
			return env.Namespace, true
		}

		value, ok := cm.Data[key]

		return value, ok
	})
	require.NoError(t, err)
	require.NoError(t, racercore.Assemble(cfg, env.Client, env.APIReader).Recover(t.Context(), env.Client))

	return cfg
}

func TestClusterCacheActivationUsesLiveReader(t *testing.T) {
	env := testEnv(t)
	require.Zero(t, planPass(t, env).Len())

	listed := false
	live := testEnv(t, cacheObject("cache"))
	env.APIReader = interceptor.NewClient(live.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			_, listed = list.(*racerv1.ClusterCacheList)
			require.True(t, listed)
			require.EqualValues(t, 1, (&client.ListOptions{}).ApplyOptions(opts).Limit)

			return c.List(ctx, list, opts...)
		},
	})
	plan := planPass(t, env)
	require.True(t, listed)
	require.Len(t, plan.Operations, 1)
	require.Equal(t, claimName, plan.Operations[0].Object.GetName())

	env.APIReader = interceptor.NewClient(live.Client.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("unavailable")
		},
	})
	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.Error(t, err)
	require.Nil(t, plan)
}

func TestControllerOnlyLifecycleWithoutSites(t *testing.T) {
	env := testEnv(t, cacheObject("first"))
	cfg := initialize(t, env)
	plan := planPass(t, env)
	persist(t, env, plan)

	for _, op := range plan.Operations {
		require.NotEqual(t, "DaemonSet", op.Object.GetKind())
		require.NotEqual(t, "NetworkPolicy", op.Object.GetKind())
		require.NotContains(t, op.Object.GetName(), "dataplane")
		require.Empty(t, op.Object.GetOwnerReferences())
	}

	deployment := &appsv1.Deployment{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	require.EqualValues(t, 3, *deployment.Spec.Replicas)
	require.Zero(t, deployment.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue())
	require.Equal(t, 1, deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntValue())
	pod := deployment.Spec.Template.Spec
	require.Equal(t, controllerName, pod.ServiceAccountName)
	require.True(t, pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken)
	require.Len(t, pod.Containers, 1)
	require.Empty(t, pod.InitContainers)
	container := pod.Containers[0]
	require.Equal(t, env.Config.Image(controllerName), container.Image)
	require.Empty(t, container.Args)
	require.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)
	require.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
	require.Equal(t, []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}, {Name: "probes", ContainerPort: 8081}, {Name: "metrics", ContainerPort: 8080}}, container.Ports)
	require.Equal(t, []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}, {Key: "tls.key", Path: "tls.key"}, {Key: caBundleKey, Path: "ca.crt"}}, pod.Volumes[0].Secret.Items)
	token := pod.Volumes[1].Projected.Sources[0].ServiceAccountToken
	require.Equal(t, racercore.ReplicationAudience, token.Audience)
	require.EqualValues(t, 3600, *token.ExpirationSeconds)
	require.EqualValues(t, 0o400, *pod.Volumes[1].Projected.DefaultMode)
	require.Equal(t, cfg.ReplicationTokenFile, container.VolumeMounts[1].MountPath+"/"+token.Path)
	require.Equal(t, controllerName+"."+env.Namespace+".svc", cfg.ReplicationServerName)
	require.EqualValues(t, 8443, cfg.ReplicationPort)

	budget := &policyv1.PodDisruptionBudget{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), budget))
	require.Nil(t, budget.Spec.MinAvailable)
	require.NotNil(t, budget.Spec.MaxUnavailable)
	require.Equal(t, intstr.FromInt32(1), *budget.Spec.MaxUnavailable)
	require.Equal(t, deployment.Spec.Selector, budget.Spec.Selector)

	service := &corev1.Service{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), service))
	require.Equal(t, deployment.Spec.Template.Labels, service.Spec.Selector)
	require.False(t, service.Spec.PublishNotReadyAddresses)

	accounts := &corev1.ServiceAccountList{}
	require.NoError(t, env.Client.List(t.Context(), accounts))
	require.Len(t, accounts.Items, 1)
	require.Equal(t, controllerName, accounts.Items[0].Name)

	before := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), before))
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("first")))
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Delete(t.Context(), deployment))
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Create(t.Context(), cacheObject("later")))
	persist(t, env, planPass(t, env))

	after := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), after))
	require.Equal(t, before, after)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
}

func TestControllerPDBReplicaOverrides(t *testing.T) {
	for _, replicas := range []int64{0, 1, 2, 3, 5} {
		t.Run(fmt.Sprintf("replicas=%d", replicas), func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)
			plan := planPass(t, env)
			entries := []override.SourcedEntry{{
				Source: override.Source{Key: "racer.yaml", Index: 0},
				Entry: override.Entry{
					Component: "racer", Kind: "Deployment",
					Patch: map[string]any{"spec": map[string]any{"replicas": replicas}},
				},
			}}
			require.NoError(t, override.ValidateErr(entries))

			report := override.Apply(plan, entries, nil)
			require.NoError(t, report.Err())
			require.Empty(t, report.Withheld)
			require.Len(t, report.Workloads, 1)
			persist(t, env, plan)

			deployment := &appsv1.Deployment{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
			require.EqualValues(t, replicas, *deployment.Spec.Replicas)
			require.Equal(t, intstr.FromInt32(0), *deployment.Spec.Strategy.RollingUpdate.MaxUnavailable)
			require.Equal(t, intstr.FromInt32(1), *deployment.Spec.Strategy.RollingUpdate.MaxSurge)

			budget := &policyv1.PodDisruptionBudget{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), budget))
			require.Nil(t, budget.Spec.MinAvailable)
			require.NotNil(t, budget.Spec.MaxUnavailable)
			require.Equal(t, intstr.FromInt32(1), *budget.Spec.MaxUnavailable)
			require.Equal(t, deployment.Spec.Selector, budget.Spec.Selector)
		})
	}
}

func TestDamagedStateFailsClosed(t *testing.T) {
	for _, target := range []string{claimName, markerName, versionName, tlsName, "wrong-version-binding", "replaced-marker", "corrupt-version"} {
		t.Run(target, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			initialize(t, env)

			var obj client.Object = &corev1.ConfigMap{}

			key := target
			switch target {
			case tlsName:
				obj = &corev1.Secret{}
			case "wrong-version-binding", "corrupt-version":
				key = versionName
			case "replaced-marker":
				key = markerName
			}

			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, key), obj))

			switch target {
			case "wrong-version-binding":
				obj.SetAnnotations(nil)
				require.NoError(t, env.Client.Update(t.Context(), obj))
			case "corrupt-version":
				obj.(*corev1.ConfigMap).Data["sequence"] = "0"
				require.NoError(t, env.Client.Update(t.Context(), obj))
			default:
				require.NoError(t, env.Client.Delete(t.Context(), obj))

				if target == "replaced-marker" {
					obj.SetResourceVersion("")
					require.NoError(t, env.Client.Create(t.Context(), obj))
				}
			}

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			require.Error(t, err)
			require.Nil(t, plan)
		})
	}
}

func TestExternalWorkloadsAreNotAdoptedOrChanged(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: "custom-system", UID: "external"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: "custom-system", UID: "external"}}
	env := testEnv(t, cacheObject("cache"), ds, sa)
	initialize(t, env)
	persist(t, env, planPass(t, env))

	actual := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(ds), actual))
	require.Equal(t, ds.Spec, actual.Spec)
	require.Equal(t, ds.UID, actual.UID)

	account := &corev1.ServiceAccount{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(sa), account))
	require.Equal(t, sa.UID, account.UID)
}

func TestControllerConfigPreservesTuningAndValidates(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

	for _, key := range []string{"RACER_DATAPLANE_IMAGE", "RACER_CONTROL_URL", "RACER_HOST_NETWORK", "RACER_BOOTSTRAP_TRUST_CONFIGMAP", "RACER_DIAGNOSTICS_PORT"} {
		require.NotContains(t, cm.Data, key)
	}

	cm.Data["RACER_HANDSHAKE_TIMEOUT"] = "9s"
	cm.Data["ADMIN_SETTING"] = "keep"
	cm.Data["RACER_CLUSTER_ID"] = "incorrect"
	delete(cm.Data, "RACER_SNAPSHOT_MAX_AGE")
	require.NoError(t, env.Client.Update(t.Context(), cm))
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	require.Equal(t, "9s", cm.Data["RACER_HANDSHAKE_TIMEOUT"])
	require.Equal(t, "keep", cm.Data["ADMIN_SETTING"])
	require.NotEqual(t, "incorrect", cm.Data["RACER_CLUSTER_ID"])
	require.NotContains(t, cm.Data, "RACER_SNAPSHOT_MAX_AGE")
	cm.Data["RACER_HANDSHAKE_TIMEOUT"] = "bad"
	require.NoError(t, env.Client.Update(t.Context(), cm))
	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.Error(t, err)
	require.Nil(t, plan)
}

func TestPoliciesGateControllerPermissions(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	plan := planPass(t, env)
	_, err := plan.ExecutionOrder()
	require.NoError(t, err)

	var policies []component.ObjectRef

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "ValidatingAdmissionPolicy" || op.Object.GetKind() == "ValidatingAdmissionPolicyBinding" {
			policies = append(policies, op.Ref())
		}

		if op.Object.GetKind() == "ValidatingAdmissionPolicy" {
			conditions, _, err := unstructured.NestedSlice(op.Object.Object, "spec", "matchConditions")
			require.NoError(t, err)
			require.Equal(t, `request.userInfo.username == "system:serviceaccount:custom-system:racer-controller"`, conditions[0].(map[string]any)["expression"])
		}
	}

	require.Len(t, policies, 2)

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "RoleBinding", "ClusterRoleBinding", "Deployment":
			for _, policy := range policies {
				require.Contains(t, op.DependsOn, policy)
			}
		}
	}

	role := &rbacv1.Role{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), role))

	for _, rule := range role.Rules {
		for _, resource := range rule.Resources {
			if resource == "daemonsets" || resource == "pods" || resource == "serviceaccounts" {
				require.ElementsMatch(t, []string{"get", "list", "watch"}, rule.Verbs)
			}
		}
	}

	clusterRole := &rbacv1.ClusterRole{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: controllerName}, clusterRole))
	require.Contains(t, clusterRole.Rules, rbacv1.PolicyRule{APIGroups: []string{"racer.unbounded-cloud.io"}, Resources: []string{"clustercaches"}, Verbs: []string{"get", "list", "watch"}})
}

func TestPolicyFailuresSkipBindingsAndStartup(t *testing.T) {
	for _, policyName := range []string{"racer-runtime-write-restriction", "racer-node-write-restriction"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			t.Run(policyName+"/"+kind, func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				initialize(t, env)

				obj := &unstructured.Unstructured{}
				obj.SetAPIVersion("admissionregistration.k8s.io/v1")
				obj.SetKind(kind)
				obj.SetName(policyName)

				if policyName == retiredRuntimeGuard {
					require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj)))
					persist(t, env, planPass(t, env))
					assertControllerBindings(t, env, true)
					require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), &appsv1.Deployment{}))

					return
				}

				require.NoError(t, env.Client.Delete(t.Context(), obj))
				containment := planPass(t, env)
				require.Len(t, containment.Operations, 2)

				for _, op := range containment.Operations {
					require.Equal(t, component.OpDelete, op.Kind)
				}

				persist(t, env, containment)
				assertControllerBindings(t, env, false)
				plan := planPass(t, env)
				original := env.Client
				env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if obj.GetObjectKind().GroupVersionKind().Kind == kind && obj.GetName() == policyName {
							return errors.New("policy unavailable")
						}

						return c.Create(ctx, obj, opts...)
					},
					Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
						data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
						require.NoError(t, err)

						if data["kind"] == kind && data["metadata"].(map[string]any)["name"] == policyName {
							return errors.New("policy unavailable")
						}

						return c.Apply(ctx, cfg, opts...)
					},
				})
				result, err := env.Execute(t.Context(), plan)
				require.NoError(t, err)
				require.Len(t, result.Failed(), 1)

				for _, op := range result.Results {
					switch op.Ref.GVK.Kind {
					case "RoleBinding", "ClusterRoleBinding", "Deployment":
						require.Equal(t, component.OpSkipped, op.Status)
					}
				}

				assertControllerBindings(t, env, false)
				env.Client = original
				persist(t, env, planPass(t, env))
				assertControllerBindings(t, env, true)
			})
		}
	}
}

func TestStartupWatchIgnoresCounterUpdates(t *testing.T) {
	env := testEnv(t)
	predicate := startupConfigPredicate(env)
	version := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName, Namespace: env.Namespace}, Data: map[string]string{"sequence": "1"}}
	next := version.DeepCopy()
	next.Data["sequence"] = "2"
	require.False(t, predicate.Update(event.UpdateEvent{ObjectOld: version, ObjectNew: next}))
	require.True(t, predicate.Create(event.CreateEvent{Object: version}))
	require.True(t, predicate.Delete(event.DeleteEvent{Object: version}))
	marker := version.DeepCopy()
	marker.Name = markerName
	changed := marker.DeepCopy()
	changed.Data["state"] = "consumed"
	require.True(t, predicate.Update(event.UpdateEvent{ObjectOld: marker, ObjectNew: changed}))
}

func TestInactiveAtEveryInstallationBoundary(t *testing.T) {
	for phase := range 6 {
		t.Run(time.Duration(phase).String(), func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			for range phase {
				persist(t, env, planPass(t, env))
			}

			require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
			require.Zero(t, planPass(t, env).Len())
		})
	}
}
