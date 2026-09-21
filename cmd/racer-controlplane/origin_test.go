// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestOriginServiceAdmission(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Service){
		"headless":    func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone },
		"pending":     func(s *corev1.Service) { s.Spec.ClusterIP = "" },
		"external":    func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName },
		"deleting":    func(s *corev1.Service) { now := metav1.Now(); s.DeletionTimestamp = &now },
		"volume":      func(s *corev1.Service) { s.Annotations = map[string]string{originServiceAnnotation: "other"} },
		"udp":         func(s *corev1.Service) { s.Spec.Ports[0].Protocol = corev1.ProtocolUDP },
		"ambiguous":   func(s *corev1.Service) { s.Spec.Ports = append(s.Spec.Ports, s.Spec.Ports[0]) },
		"dns":         func(s *corev1.Service) { s.Spec.ClusterIPs = []string{"origin.ns.svc"} },
		"mapped":      func(s *corev1.Service) { s.Spec.ClusterIPs = []string{"::ffff:10.0.0.1"} },
		"unspecified": func(s *corev1.Service) { s.Spec.ClusterIPs = []string{"0.0.0.0"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := originFixture()
			mutate(s)

			if _, err := resolveOrigin(s, "http"); err == nil {
				t.Fatal("invalid origin admitted")
			}
		})
	}

	s := originFixture()
	s.Spec.Ports[0].TargetPort = intstr.FromInt32(9999)

	named, err := resolveOrigin(s, "http")
	if err != nil {
		t.Fatal(err)
	}

	numeric, err := resolveOrigin(s, "8080")
	if err != nil || named != numeric || named.IPv4 != "10.100.0.2:8080" || named.IPv6 != "[fd00::2]:8080" {
		t.Fatalf("Service port selection: %+v %v", named, err)
	}

	if _, err := resolveOrigin(s, "9999"); err == nil {
		t.Fatal("targetPort selected")
	}
}

func TestOriginFamilyAndBootstrap(t *testing.T) {
	for _, ip := range []string{"10.1.1.1", "fd00::1"} {
		n, p, v := fixtures()
		p.Status.PodIP = ip
		o := originFixture()

		g, _, err := buildGenerationReserved("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []corev1.Service{*v, *o}, nil)
		if err != nil {
			t.Fatal(err)
		}

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		address, err := bootstrapAddress(o, "http", ip)
		if err != nil {
			t.Fatal(err)
		}

		volume := index.snapshot(identity("node", "node-uid")).Volumes[0]
		if volume.OriginAddress != address || volume.OriginIdentity != "ns/origin:8080" {
			t.Fatalf("family mismatch: %+v", volume)
		}

		o.Spec.ClusterIPs = []string{"10.100.0.2"}
		if ip == "fd00::1" {
			if _, _, err := buildGenerationReserved("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []corev1.Service{*v, *o}, nil); err == nil {
				t.Fatal("missing origin family accepted")
			}

			if _, err := bootstrapAddress(o, "http", ip); err == nil {
				t.Fatal("missing bootstrap family accepted")
			}
		}
	}
}

func TestOriginDependencyUpdatesAndLastGood(t *testing.T) {
	ctx := context.Background()
	n, p, v := fixtures()
	v.Annotations[originNamespaceAnnotation] = "remote"
	o := originFixture()
	o.Namespace = "remote"
	o.Annotations = map[string]string{universeAnnotation: "origin-universe"}
	c := fakeKube(n, p, v, o)
	r := newTestReconciler(c)
	requests := r.serviceRequests(ctx, o)
	found := false

	for _, request := range requests {
		if request.Name == "default" {
			found = true
		}
	}

	if !found {
		t.Fatal("cross-universe dependency not enqueued")
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	step := func() {
		t.Helper()

		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}

		index, err := indexGeneration(r.loaded["default"])
		if err != nil {
			t.Fatal(err)
		}

		rollout, err := r.server.rolloutFor(ctx, index)
		if err != nil {
			t.Fatal(err)
		}

		if err := r.server.persistPhase(ctx, "default", rollout, 4); err != nil {
			t.Fatal(err)
		}
	}
	step()

	first := r.loaded["default"]

	if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
		t.Fatal(err)
	}

	o.Spec.ClusterIP = "10.100.0.3"

	o.Spec.ClusterIPs[0] = o.Spec.ClusterIP
	if err := c.Update(ctx, o); err != nil {
		t.Fatal(err)
	}

	step()

	second := r.loaded["default"]
	if second.Revision != first.Revision+1 || second.Volume.Origin.Identity != first.Volume.Origin.Identity || second.Volume.Origin.IPv4 != "10.100.0.3:8080" {
		t.Fatal("address update did not preserve identity and advance revision")
	}

	if err := c.Delete(ctx, o); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("missing origin accepted")
	}

	if r.loaded["default"] != second {
		t.Fatal("invalid proposal replaced last good")
	}

	r = newTestReconciler(c)
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("missing origin accepted after restart")
	}

	if r.loaded["default"].Revision != second.Revision {
		t.Fatal("restart lost last good")
	}

	o.ResourceVersion = ""

	o.UID = "recreated"
	if err := c.Create(ctx, o); err != nil {
		t.Fatal(err)
	}

	step()

	if r.loaded["default"].Volume.Origin.Identity != first.Volume.Origin.Identity {
		t.Fatal("recreation changed logical identity")
	}
}
