// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, rbacv1.AddToScheme, racerv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	return &component.Env{Client: c, APIReader: c, Scheme: scheme, Namespace: "custom-system", Config: component.Config{ImageRegistry: "mirror.test/unbounded", ImageTag: "v1"}}
}

func cache(name string) *racerv1.ClusterCache {
	return &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func planPass(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()

	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	if err != nil {
		t.Fatal(err)
	}

	return plan
}

// Persist plans through a simple fake API. SSA itself is tested by the shared
// executor; assign a server UID here because the fake client does not do so.
func persist(t *testing.T, env *component.Env, plan *component.Plan) {
	t.Helper()

	for _, op := range plan.Operations {
		obj := op.Object.DeepCopy()
		current := obj.DeepCopy()

		err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), current)
		if apierrors.IsNotFound(err) {
			obj.SetUID(types.UID("uid-" + client.ObjectKeyFromObject(obj).Name))

			if err := env.Client.Create(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else if op.Kind != component.OpCreateIfAbsent {
			obj.SetResourceVersion(current.GetResourceVersion())
			obj.SetUID(current.GetUID())

			if err := env.Client.Update(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func initialize(t *testing.T, env *component.Env) *corev1.ConfigMap {
	t.Helper()

	first := planPass(t, env)
	if first.Len() != 1 || first.Operations[0].Object.GetName() != markerName {
		t.Fatalf("identity was not staged alone: %s", first.Summary())
	}
	// Planning must not persist anything.
	if err := env.Client.Get(t.Context(), objectKey(env, markerName), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("planning wrote marker: %v", err)
	}

	persist(t, env, first)
	persist(t, env, planPass(t, env)) // TLS Create only.

	setup := planPass(t, env)
	if strings.Contains(setup.Summary(), "Deployment/") {
		t.Fatal("started controller before initialization")
	}

	persist(t, env, setup)

	marker := &corev1.ConfigMap{}
	if err := env.Client.Get(t.Context(), objectKey(env, markerName), marker); err != nil {
		t.Fatal(err)
	}

	r := &racercore.TopologyReconciler{Client: env.Client, APIReader: env.Client, Config: racercore.Config{
		Namespace: env.Namespace, Cluster: wire.ClusterID(marker.Data["cluster"]), InstallationConfigMapName: markerName, VersionConfigMapName: "racer-version",
	}}
	if err := r.InitializeVersion(t.Context()); err != nil {
		t.Fatal(err)
	}

	return marker
}

func TestLifecycleWithoutSites(t *testing.T) {
	env := testEnv(t)
	if p := planPass(t, env); p.Len() != 0 {
		t.Fatal(p.Summary())
	}

	if err := env.Client.Create(t.Context(), cache("first")); err != nil {
		t.Fatal(err)
	}

	marker := initialize(t, env)

	plan := planPass(t, env)
	if !strings.Contains(plan.Summary(), "Deployment/custom-system/racer-controller") || strings.Contains(plan.Summary(), "DaemonSet/") {
		t.Fatal(plan.Summary())
	}

	persist(t, env, plan)

	deployment := &appsv1.Deployment{}
	if err := env.Client.Get(t.Context(), objectKey(env, controllerName), deployment); err != nil {
		t.Fatal(err)
	}

	if deployment.Spec.Template.Spec.Containers[0].Image != env.Config.Image(controllerName) {
		t.Fatal("controller image not version matched")
	}

	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("leader-only readiness would stall a rolling update")
	}

	for _, op := range plan.Operations {
		if len(op.Object.GetOwnerReferences()) != 0 {
			t.Fatal("installation must survive cache deletion")
		}
	}

	config := &corev1.ConfigMap{}
	if err := env.Client.Get(t.Context(), objectKey(env, configName), config); err != nil {
		t.Fatal(err)
	}

	if config.Data["RACER_CONTROL_URL"] != "https://racer-controller.custom-system.svc:8443" || config.Data["RACER_DATAPLANE_IMAGE"] != env.Config.Image("racer-dataplane") {
		t.Fatal(config.Data)
	}

	job := &batchv1.Job{}
	if err := env.Client.Get(t.Context(), objectKey(env, jobName), job); err != nil {
		t.Fatal(err)
	}

	if *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("initializer can restart")
	}

	if err := env.Client.Create(t.Context(), cache("second")); err != nil {
		t.Fatal(err)
	}

	if err := env.Client.Delete(t.Context(), cache("first")); err != nil {
		t.Fatal(err)
	}

	if planPass(t, env).Len() == 0 {
		t.Fatal("stopped with remaining cache")
	}

	if err := env.Client.Delete(t.Context(), cache("second")); err != nil {
		t.Fatal(err)
	}

	if p := planPass(t, env); p.Len() != 0 {
		t.Fatal(p.Summary())
	}

	if err := env.Client.Delete(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}

	if p := planPass(t, env); p.Len() != 0 {
		t.Fatal("recreated manually removed workload")
	}

	before := &corev1.ConfigMap{}
	if err := env.Client.Get(t.Context(), objectKey(env, "racer-version"), before); err != nil {
		t.Fatal(err)
	}

	if err := env.Client.Create(t.Context(), cache("later")); err != nil {
		t.Fatal(err)
	}

	persist(t, env, planPass(t, env))

	after := &corev1.ConfigMap{}
	if err := env.Client.Get(t.Context(), objectKey(env, "racer-version"), after); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(before, after) || after.Data["cluster"] != marker.Data["cluster"] {
		t.Fatal("reinstallation reset durable state")
	}

	if err := env.Client.Get(t.Context(), objectKey(env, controllerName), deployment); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedStateFailsClosed(t *testing.T) {
	for _, target := range []string{markerName, "racer-version", "corrupt-version", "wrong-binding"} {
		t.Run(target, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)

			cm := &corev1.ConfigMap{}

			key := target
			if target == "corrupt-version" || target == "wrong-binding" {
				key = "racer-version"
			}

			if err := env.Client.Get(t.Context(), objectKey(env, key), cm); err != nil {
				t.Fatal(err)
			}

			if target == "corrupt-version" {
				cm.Data["sequence"] = "0"
				if err := env.Client.Update(t.Context(), cm); err != nil {
					t.Fatal(err)
				}
			} else if target == "wrong-binding" {
				cm.Annotations = nil
				if err := env.Client.Update(t.Context(), cm); err != nil {
					t.Fatal(err)
				}
			} else if err := env.Client.Delete(t.Context(), cm); err != nil {
				t.Fatal(err)
			}

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			if err == nil || plan.Len() != 0 {
				t.Fatalf("damaged state was repaired: %v, %s", err, plan.Summary())
			}
		})
	}
}

func TestServingTLSRenewal(t *testing.T) {
	secret, err := newTLS("custom-system", time.Now().Add(-340*day))
	if err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, secret)
	plan := component.NewPlan()

	result, err := planTLS(t.Context(), env, plan)
	if err != nil || result != nil || plan.Len() != 1 {
		t.Fatalf("renewal: %v %v %s", result, err, plan.Summary())
	}

	persist(t, env, plan)

	result, err = planTLS(t.Context(), env, component.NewPlan())
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(result.Data["ca.key"], secret.Data["ca.key"]) {
		t.Fatal("renewal changed CA key")
	}

	pair, err := tls.X509KeyPair(result.Data[corev1.TLSCertKey], result.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(secret.Data["ca.crt"])

	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "racer-controller.custom-system.svc", Roots: roots}); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationFailures(t *testing.T) {
	for _, scenario := range []string{"failed-job", "foreign-job", "active-consumed", "consumed-no-version", "fresh-with-version"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			for range 3 {
				persist(t, env, planPass(t, env))
			}

			job := &batchv1.Job{}
			if err := env.Client.Get(t.Context(), objectKey(env, jobName), job); err != nil {
				t.Fatal(err)
			}

			marker := &corev1.ConfigMap{}
			if err := env.Client.Get(t.Context(), objectKey(env, markerName), marker); err != nil {
				t.Fatal(err)
			}

			switch scenario {
			case "failed-job":
				job.Status.Failed = 1
			case "foreign-job":
				job.Annotations = nil
			case "active-consumed", "consumed-no-version":
				marker.Data["state"] = "consumed"
				immutable := true

				marker.Immutable = &immutable
				if err := env.Client.Update(t.Context(), marker); err != nil {
					t.Fatal(err)
				}

				if scenario == "active-consumed" {
					job.Status.Active = 1
				}
			case "fresh-with-version":
				if err := env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.Namespace, Name: "racer-version"}}); err != nil {
					t.Fatal(err)
				}
			}

			if err := env.Client.Update(t.Context(), job); err != nil {
				t.Fatal(err)
			}

			if scenario == "failed-job" || scenario == "active-consumed" {
				if scenario == "failed-job" {
					job.Status.Failed = 1
				} else {
					job.Status.Active = 1
				}

				if err := env.Client.Status().Update(t.Context(), job); err != nil {
					t.Fatal(err)
				}
			}

			plan, result, err := (Component{}).Plan(t.Context(), env, nil)
			if scenario == "active-consumed" {
				if err != nil || result.RequeueAfter <= 0 || plan.Len() != 0 {
					t.Fatalf("active initializer: %+v %v", result, err)
				}
			} else if err == nil || plan.Len() != 0 {
				t.Fatalf("accepted unsafe initialization: %v %s", err, plan.Summary())
			}
		})
	}
}

func TestCacheListFailureDoesNotInstall(t *testing.T) {
	env := testEnv(t)
	env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return errors.New("unavailable")
		},
	})

	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	if err == nil || plan.Len() != 0 {
		t.Fatalf("list failure ignored: %v", err)
	}
}

func TestTLSMissingAndInvalidFailClosed(t *testing.T) {
	for _, scenario := range []string{"missing", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)

			secret := &corev1.Secret{}
			if err := env.Client.Get(t.Context(), objectKey(env, tlsName), secret); err != nil {
				t.Fatal(err)
			}

			if scenario == "missing" {
				if err := env.Client.Delete(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			} else {
				secret.Data["tls.key"] = []byte("invalid")
				if err := env.Client.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
			}

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			if err == nil || plan.Len() != 0 {
				t.Fatalf("replaced damaged TLS state: %v", err)
			}
		})
	}
}
