// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func TestRoutingMigrationUsesLiveNamedReadiness(t *testing.T) {
	for _, test := range []struct {
		name   string
		check  string
		ready  bool
		legacy bool
	}{
		{"old leader", "leader-tls", true, true},
		{"old standby", "leader-tls", false, true},
		{"warm standby", "replica-tls", true, false},
		{"starting warm replica", "replica-tls", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &healthz.Handler{Checks: map[string]healthz.Checker{test.check: func(*http.Request) error {
				if !test.ready {
					return errors.New("not ready")
				}

				return nil
			}}}

			server := httptest.NewServer(http.StripPrefix("/readyz", handler))
			defer server.Close()

			host, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}

			p, err := strconv.Atoi(port)
			if err != nil {
				t.Fatal(err)
			}

			deployment := controlDeployment("custom", component.Config{})
			deployment.UID = "deployment"
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "custom", UID: "rs", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: ptr.To(true)}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "custom", UID: "pod", Labels: map[string]string{componentLabel: controlPlaneName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{ServiceAccountName: controlPlaneName, Containers: []corev1.Container{{Name: "controller", Ports: []corev1.ContainerPort{{Name: "health", ContainerPort: int32(p)}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host}}
			service := controlService("custom")
			delete(service.Spec.Selector, racermeta.MetadataPrefix+"serving-leader")
			env := testEnv(t, interceptor.Funcs{}, deployment, rs, pod, service)
			plan := component.NewPlan()

			deps, err := planRoutingMigration(t.Context(), env, plan)
			if err != nil {
				t.Fatal(err)
			}

			if (len(deps) == 1) != test.legacy {
				t.Fatalf("legacy=%v dependencies=%v", test.legacy, deps)
			}

			if _, err := env.Execute(t.Context(), plan); err != nil {
				t.Fatal(err)
			}

			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}

			selected := labels.SelectorFromSet(controlService("custom").Spec.Selector).Matches(labels.Set(pod.Labels))
			if selected != test.legacy {
				t.Fatalf("selector lost legacy replica or included warm standby: %v", selected)
			}
			// Force the actual Pod patch to fail. The real executor must skip the
			// dependent Service apply, retaining the existing selector.
			if test.legacy {
				plan.Add(component.Operation{Kind: component.OpApply, Object: resourceObject(controlService("custom")), Component: controlPlaneName, DependsOn: deps})

				attemptedService := false

				watched, ok := env.Client.(client.WithWatch)
				if !ok {
					t.Fatal("fake client lacks Watch")
				}

				env.Client = interceptor.NewClient(watched, interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
					if obj.GetObjectKind().GroupVersionKind().Kind == "Service" {
						attemptedService = true
					}

					return errors.New("injected patch conflict")
				}})

				_, _ = env.Execute(t.Context(), plan)
				if attemptedService {
					t.Fatal("Service changed after legacy route patch failed")
				}
			}
		})
	}
}

func TestRoutingMigrationRejectsUnknownReadiness(t *testing.T) {
	for _, body := range []string{"ok", "[+]ping ok\n", "[+]leader-tls ok\n[+]replica-tls ok\n", "[+]leader-tls excluded: ok\n"} {
		if _, err := classifyReadiness(http.StatusOK, body); err == nil {
			t.Fatalf("accepted unknown contract %q", body)
		}
	}
}
