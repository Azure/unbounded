// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/internal/racer/workload"
)

func TestScope(t *testing.T) {
	for _, tc := range []struct {
		name, component, namespace string
		kind                       component.OpKind
		allowed                    bool
	}{
		{"racer-config", "racer", namespace, component.OpApply, true},
		{"unbounded-component-overrides", "racer", namespace, component.OpApply, false},
		{"racer-config", "net", namespace, component.OpApply, false},
		{"racer-config", "racer", "other", component.OpApply, false},
		{"racer-config", "racer", namespace, component.OpDelete, false},
	} {
		plan := component.NewPlan()
		plan.Add(component.Operation{Component: tc.component, Kind: tc.kind, Object: component.ToUnstructured(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: tc.namespace}})})

		if (scoped(plan) == nil) != tc.allowed {
			t.Fatalf("unexpected scope verdict: %+v", tc)
		}
	}
}

func TestExecutePlan(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	for _, fail := range []bool{false, true} {
		writeErr := errors.New("write refused")
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if fail {
					return writeErr
				}

				return c.Create(ctx, obj, opts...)
			},
		}).Build()
		plan := component.NewPlan()
		plan.Add(component.Operation{Component: "racer", Kind: component.OpCreateIfAbsent, Object: component.ToUnstructured(&corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "racer-config", Namespace: namespace},
		})})

		err := executePlan(t.Context(), &component.Env{Client: c}, plan)
		if fail && !errors.Is(err, writeErr) || !fail && err != nil {
			t.Fatalf("fail=%v: unexpected execution error: %v", fail, err)
		}
	}
}

func TestWorkloadsReady(t *testing.T) {
	controller := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: appsv1.DeploymentStatus{
		ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3,
	}}

	dp := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: appsv1.DaemonSetStatus{
		ObservedGeneration: 2, DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberReady: 2,
	}}
	if !workloadsReady(controller, dp) {
		t.Fatal("fully rolled out workloads are not ready")
	}

	for _, change := range []func(*appsv1.Deployment, *appsv1.DaemonSet){
		func(c *appsv1.Deployment, _ *appsv1.DaemonSet) { c.Status.ObservedGeneration = 1 },
		func(c *appsv1.Deployment, _ *appsv1.DaemonSet) { c.Status.UpdatedReplicas = 1 },
		func(c *appsv1.Deployment, _ *appsv1.DaemonSet) { c.Status.Replicas = 4 },
		func(c *appsv1.Deployment, _ *appsv1.DaemonSet) { c.Status.AvailableReplicas = 2 },
		func(_ *appsv1.Deployment, d *appsv1.DaemonSet) { d.Status.ObservedGeneration = 1 },
		func(_ *appsv1.Deployment, d *appsv1.DaemonSet) { d.Status.UpdatedNumberScheduled = 1 },
		func(_ *appsv1.Deployment, d *appsv1.DaemonSet) { d.Status.NumberReady = 1 },
		func(_ *appsv1.Deployment, d *appsv1.DaemonSet) { d.Status.DesiredNumberScheduled = 0 },
	} {
		c, d := controller.DeepCopy(), dp.DeepCopy()
		change(c, d)

		if workloadsReady(c, d) {
			t.Fatalf("incomplete rollout reported ready: %+v %+v", c.Status, d.Status)
		}
	}
}

func TestScopeProtectedObjects(t *testing.T) {
	for _, kind := range []string{"Node", "Namespace"} {
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("v1")
		obj.SetKind(kind)
		obj.SetName("racer")

		plan := component.NewPlan()
		plan.Add(component.Operation{Component: "racer", Object: obj})

		if scoped(plan) == nil {
			t.Fatalf("allowed protected kind %s", kind)
		}
	}

	plan := component.NewPlan()
	plan.Add(component.Operation{Component: "racer"})

	if scoped(plan) == nil {
		t.Fatal("allowed a missing object")
	}
}

func TestAuthoritativeDataplaneOverrides(t *testing.T) {
	ds, err := workload.DesiredDaemonSet(workload.Config{
		Cluster: wire.ClusterID("11111111-1111-1111-1111-111111111111"), Namespace: namespace,
		ControlURL: "https://racer-controller.unbounded-system.svc:8443", DataplaneImage: "example:old",
		PeerPort: 8082, BootstrapTrustConfigMap: "racer-bootstrap-trust",
		DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile("../../../deploy/racer-loadgen/zipf/racer-overrides.yaml")
	if err != nil {
		t.Fatal(err)
	}

	entries, problems, err := override.Parse(map[string]string{"local.yaml": string(data)})
	if err != nil || len(problems) != 0 {
		t.Fatalf("%v %v", err, problems)
	}

	plan := component.NewPlan()
	plan.Add(component.Operation{Component: "racer", Kind: component.OpApply, Object: component.ToUnstructured(ds), Overridable: true})

	if report := override.Apply(plan, entries, nil); report.Failed() {
		t.Fatal(report.Err())
	}

	obj := plan.Operations[0].Object.Object

	annotations, _, err := unstructured.NestedStringMap(obj, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9090",
		"prometheus.io/path":   "/metrics",
	} {
		if annotations[key] != want {
			t.Fatalf("annotation %s = %q, want %q", key, annotations[key], want)
		}
	}

	pool, _, _ := unstructured.NestedString(obj, "spec", "template", "spec", "nodeSelector", "agentpool")
	if pool != "ddsv6" {
		t.Fatal(pool)
	}

	containers, _, _ := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")

	dp := containers[0].(map[string]any)
	if dp["image"] != "ghcr.io/azure/racer-dataplane:"+imageTag {
		t.Fatal(dp["image"])
	}

	if limits, found, _ := unstructured.NestedMap(dp, "resources", "limits"); found && len(limits) != 0 {
		t.Fatal(limits)
	}

	env, _, err := unstructured.NestedSlice(dp, "env")
	if err != nil {
		t.Fatal(err)
	}

	values := map[string]string{}

	for _, item := range env {
		entry := item.(map[string]any)
		if value, ok := entry["value"].(string); ok {
			values[entry["name"].(string)] = value
		}
	}

	for key, want := range map[string]string{
		"RACER_PLAINTEXT_BYTES":       "4294967296",
		"RACER_CIPHERTEXT_BYTES":      "8589934592",
		"RACER_DIRTY_BYTES":           "2147483648",
		"RACER_REGISTERED_BYTES":      "2147483648",
		"RACER_REQUEST_CONTEXT_BYTES": "268435456",
		"RACER_FLIGHTS":               "512",
		"RACER_QUEUE_ENTRIES":         "4096",
		"RACER_CLIENT_CONNECTIONS":    "1024",
		"RACER_PIPES":                 "256",
		"RACER_RELAY_TRANSFERS":       "256",
		"RACER_ADMISSION_MODE":        "disabled",
	} {
		if values[key] != want {
			t.Fatalf("%s = %q, want %q", key, values[key], want)
		}
	}

	if _, capped := values["RACER_MAX_THREADS"]; capped {
		t.Fatal("worker sizing must remain automatic")
	}

	if values["RACER_CLUSTER_ID"] == "" || values["RACER_CONTROL_ENDPOINT"] == "" {
		t.Fatal("tuning must preserve bootstrap identity and control endpoint")
	}
}
