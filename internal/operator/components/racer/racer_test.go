// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, racerv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			// The fake API does not allocate server UIDs.
			obj.SetUID(types.UID(uuid.NewString()))
			return c.Create(ctx, obj, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			// Exercise the real plan executor; emulate only SSA persistence. The
			// envtest suite below exercises actual API defaulting and admission.
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

			obj.SetResourceVersion(current.GetResourceVersion())
			obj.SetUID(current.GetUID())

			return c.Update(ctx, obj)
		},
	}).Build()

	return &component.Env{Client: c, APIReader: c, Scheme: scheme, Namespace: "custom-system", Config: component.Config{ImageRegistry: "mirror.test/unbounded", ImageTag: "v1"}}
}

func cache(name string) *racerv1.ClusterCache {
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

func configuration(t *testing.T, env *component.Env) racercore.Config {
	t.Helper()

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

	for key, value := range cm.Data {
		t.Setenv(key, value)
	}

	t.Setenv("POD_NAMESPACE", env.Namespace)

	cfg, err := racercore.LoadConfig()
	require.NoError(t, err)

	return cfg
}

func initialize(t *testing.T, env *component.Env) racercore.Config {
	t.Helper()

	for range 4 {
		plan := planPass(t, env)
		require.NotContains(t, plan.Summary(), "Deployment/")
		persist(t, env, plan)
	}

	cfg := configuration(t, env)
	r := &racercore.TopologyReconciler{Client: env.Client, APIReader: env.APIReader, Config: cfg}
	require.NoError(t, r.InitializeVersion(t.Context()))

	return cfg
}

func TestLifecycleWithoutSites(t *testing.T) {
	env := testEnv(t)
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Create(t.Context(), cache("first")))
	cfg := initialize(t, env)
	plan := planPass(t, env)
	require.Contains(t, plan.Summary(), "Deployment/custom-system/racer-controller [overridable]")
	require.Contains(t, plan.Summary(), "DaemonSet/custom-system/racer-dataplane [overridable]")
	persist(t, env, plan)

	deployment := &appsv1.Deployment{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	require.Equal(t, env.Config.Image(controllerName), deployment.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, appsv1.RecreateDeploymentStrategyType, deployment.Spec.Strategy.Type)
	require.Nil(t, deployment.Spec.Strategy.RollingUpdate)
	require.Len(t, deployment.Spec.Template.Spec.Volumes[0].Secret.Items, 2)

	for _, item := range deployment.Spec.Template.Spec.Volumes[0].Secret.Items {
		require.NotEqual(t, "ca.key", item.Key)
	}

	serving := &corev1.Secret{}
	trust := &corev1.ConfigMap{}

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), serving))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
	require.Equal(t, string(serving.Data["ca.crt"]), trust.Data["ca.crt"])
	cert, err := tls.X509KeyPair(serving.Data[corev1.TLSCertKey], serving.Data[corev1.TLSPrivateKeyKey])
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM([]byte(trust.Data["ca.crt"])))
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: controllerName + "." + env.Namespace + ".svc", Roots: roots})
	require.NoError(t, err)
	require.Equal(t, "https://racer-controller.custom-system.svc:8443", cfg.ControlURL)

	job := &batchv1.Job{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, jobName), job))
	require.Zero(t, *job.Spec.BackoffLimit)
	require.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
	require.Equal(t, []string{"initialize"}, job.Spec.Template.Spec.Containers[0].Args)

	for _, op := range plan.Operations {
		require.Empty(t, op.Object.GetOwnerReferences())
	}

	// Use the production controller with precisely the operator's configuration.
	app := racercore.Assemble(cfg, env.Client, env.APIReader)
	_, err = app.Keyring.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)

	ds := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, "racer-dataplane"), ds))
	require.Equal(t, env.Config.Image("racer-dataplane"), ds.Spec.Template.Spec.Containers[0].Image)

	for _, name := range []string{"racer-issuer", "racer-keyring"} {
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, name), &corev1.Secret{}))
	}

	before := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), before))
	require.NoError(t, env.Client.Delete(t.Context(), cache("first")))
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Delete(t.Context(), deployment))
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Create(t.Context(), cache("later")))
	persist(t, env, planPass(t, env))

	after := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), after))
	require.Equal(t, before, after)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
}

func TestDamagedStateFailsClosed(t *testing.T) {
	for _, target := range []string{claimName, markerName, versionName, tlsName, "corrupt-version", "wrong-binding"} {
		t.Run(target, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)

			var obj client.Object = &corev1.ConfigMap{}

			key := target
			if target == tlsName {
				obj = &corev1.Secret{}
			}

			if target == "corrupt-version" || target == "wrong-binding" {
				key = versionName
			}

			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, key), obj))

			switch target {
			case "corrupt-version":
				obj.(*corev1.ConfigMap).Data["sequence"] = "0"
				require.NoError(t, env.Client.Update(t.Context(), obj))
			case "wrong-binding":
				obj.SetAnnotations(nil)
				require.NoError(t, env.Client.Update(t.Context(), obj))
			default:
				require.NoError(t, env.Client.Delete(t.Context(), obj))
			}

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestClaimCrashAndConflict(t *testing.T) {
	for _, failure := range []string{"conflict", "lost-marker-create", "lost-CAS-response"} {
		t.Run(failure, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			persist(t, env, planPass(t, env))
			plan := planPass(t, env)
			require.Len(t, plan.Operations, 2)

			writer := interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if failure == "conflict" {
						return apierrors.NewConflict(corev1.Resource("configmaps"), claimName, errors.New("concurrent writer"))
					}

					if failure == "lost-CAS-response" {
						require.NoError(t, c.Patch(ctx, obj, patch, opts...))
						return errors.New("response lost after claim consumption")
					}

					return c.Patch(ctx, obj, patch, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return errors.New("lost create")
				},
			})
			executor := *env
			executor.Client = writer
			result, err := executor.Execute(t.Context(), plan)
			require.NoError(t, err)
			require.Len(t, result.Results, 2)
			require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, markerName), &corev1.ConfigMap{})))

			if failure == "conflict" {
				persist(t, env, planPass(t, env))
			} else {
				next, _, err := (Component{}).Plan(t.Context(), env, nil)
				require.Error(t, err)
				require.Zero(t, next.Len())
			}
		})
	}
}

func TestStandaloneAndReadFailure(t *testing.T) {
	env := testEnv(t, cache("cache"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: "custom-system"}})
	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "standalone")
	require.Zero(t, plan.Len())

	env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("unavailable")
		},
	})
	plan, _, err = (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "unavailable")
	require.Zero(t, plan.Len())
}

func TestInitializerDependsOnPrerequisites(t *testing.T) {
	env := testEnv(t, cache("cache"))
	for range 3 {
		persist(t, env, planPass(t, env))
	}

	plan := planPass(t, env)
	env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("RBAC forbidden")
		},
	})
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)

	for _, op := range result.Results {
		if op.Ref.Name == jobName {
			require.Equal(t, component.OpSkipped, op.Status)
		}
	}

	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, jobName), &batchv1.Job{})))
}

func TestServingTLSRenewal(t *testing.T) {
	for _, age := range []time.Duration{340 * day, 9*365*day + day} {
		t.Run(age.String(), func(t *testing.T) {
			secret, err := newTLS("custom-system", time.Now().Add(-age))
			require.NoError(t, err)
			env := testEnv(t, secret)
			plan := component.NewPlan()
			result, err := planTLS(t.Context(), env, plan, false)
			require.NoError(t, err)
			require.Nil(t, result)
			persist(t, env, plan)
			result, err = planTLS(t.Context(), env, component.NewPlan(), false)
			require.NoError(t, err)
			require.Equal(t, secret.Data["ca.key"], result.Data["ca.key"])
			pair, err := tls.X509KeyPair(result.Data[corev1.TLSCertKey], result.Data[corev1.TLSPrivateKeyKey])
			require.NoError(t, err)
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			require.NoError(t, err)

			roots := x509.NewCertPool()
			require.True(t, roots.AppendCertsFromPEM(secret.Data["ca.crt"]))
			_, err = leaf.Verify(x509.VerifyOptions{DNSName: controllerName + ".custom-system.svc", Roots: roots})
			require.NoError(t, err)
		})
	}
}

func TestInitializationFailureStates(t *testing.T) {
	for _, scenario := range []string{"failed", "foreign", "completed-fresh", "consumed-active", "consumed-missing", "fresh-version"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			for range 4 {
				persist(t, env, planPass(t, env))
			}

			job := &batchv1.Job{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, jobName), job))

			marker := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))

			switch scenario {
			case "failed":
				job.Status.Failed = 1
				require.NoError(t, env.Client.Status().Update(t.Context(), job))
			case "foreign":
				job.Annotations = nil
				require.NoError(t, env.Client.Update(t.Context(), job))
			case "completed-fresh":
				job.Status.Succeeded = 1
				require.NoError(t, env.Client.Status().Update(t.Context(), job))
			case "consumed-active", "consumed-missing":
				marker.Data["state"] = "consumed"
				immutable := true
				marker.Immutable = &immutable
				require.NoError(t, env.Client.Update(t.Context(), marker))

				if scenario == "consumed-active" {
					job.Status.Active = 1
					require.NoError(t, env.Client.Status().Update(t.Context(), job))
				}
			case "fresh-version":
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName, Namespace: env.Namespace}}))
			}

			plan, result, err := (Component{}).Plan(t.Context(), env, nil)
			require.Zero(t, plan.Len())

			if scenario == "consumed-active" {
				require.NoError(t, err)
				require.Positive(t, result.RequeueAfter)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestTLSRejectsInvalidCredentials(t *testing.T) {
	for _, scenario := range []string{"bad-key", "wrong-host", "future", "missing-with-trust"} {
		t.Run(scenario, func(t *testing.T) {
			namespace, now := "custom-system", time.Now()
			if scenario == "wrong-host" {
				namespace = "elsewhere"
			}

			if scenario == "future" {
				now = now.Add(2 * time.Hour)
			}

			secret, err := newTLS(namespace, now)
			require.NoError(t, err)

			secret.Namespace = "custom-system"
			if scenario == "bad-key" {
				secret.Data[corev1.TLSPrivateKeyKey] = []byte("invalid")
			}

			env := testEnv(t, secret)
			if scenario == "missing-with-trust" {
				require.NoError(t, env.Client.Delete(t.Context(), secret))
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace}}))
			}

			plan := component.NewPlan()
			_, err = planTLS(t.Context(), env, plan, true)
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestConfigEditsPersistAndRollWorkloads(t *testing.T) {
	env := testEnv(t, cache("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	for _, test := range []struct {
		config, workload string
		object           client.Object
	}{
		{configName, controllerName, &appsv1.Deployment{}},
		{dataplaneConfigName, dataplaneName, &appsv1.DaemonSet{}},
	} {
		cm := &corev1.ConfigMap{}
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, test.config), cm))
		cm.Data["ADMIN_SETTING"] = "preserved"

		cm.BinaryData = map[string][]byte{"admin.bin": {1, 2, 3}}
		if test.config == dataplaneConfigName {
			cm.Data["RACER_MAX_THREADS"] = "2"
		}

		require.NoError(t, env.Client.Update(t.Context(), cm))
		want := component.ConfigMapPayloadHash(cm)

		for range 2 {
			persist(t, env, planPass(t, env))
		}

		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, test.config), cm))
		require.Equal(t, want, component.ConfigMapPayloadHash(cm))
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, test.workload), test.object))

		var annotations map[string]string

		switch obj := test.object.(type) {
		case *appsv1.Deployment:
			annotations = obj.Spec.Template.Annotations
		case *appsv1.DaemonSet:
			annotations = obj.Spec.Template.Annotations
			container := obj.Spec.Template.Spec.Containers[0]
			require.Equal(t, dataplaneConfigName, container.EnvFrom[0].ConfigMapRef.Name)
			require.Equal(t, "1Gi", container.Resources.Requests.Memory().String())
			require.Empty(t, container.Resources.Limits)
		}

		require.Equal(t, want, annotations["unbounded-cloud.io/racer-config-hash"])
	}
}

func TestAdmissionFailureGatesBindingAndWorkloads(t *testing.T) {
	for _, target := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		t.Run(target, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)

			policy := &unstructured.Unstructured{}
			policy.SetAPIVersion("admissionregistration.k8s.io/v1")
			policy.SetKind(target)
			policy.SetName("racer-runtime-write-restriction")
			require.NoError(t, env.Client.Delete(t.Context(), policy))
			plan := planPass(t, env)
			env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
					require.NoError(t, err)

					if data["kind"] == target {
						return errors.New("policy unavailable")
					}

					return c.Apply(ctx, cfg, opts...)
				},
			})
			result, err := env.Execute(t.Context(), plan)
			require.NoError(t, err)

			for _, op := range result.Results {
				if op.Ref.GVK.Kind == "Deployment" || op.Ref.GVK.Kind == "DaemonSet" || op.Ref.GVK.Kind == "RoleBinding" {
					require.Equal(t, component.OpSkipped, op.Status, "%s", op.Ref)
				}
			}
		})
	}
}
