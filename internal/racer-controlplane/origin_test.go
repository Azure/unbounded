// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func originFixture() *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "racer-controlplane", Namespace: "ns"}, Spec: corev1.ServiceSpec{ClusterIP: "10.100.0.2", ClusterIPs: []string{"10.100.0.2", "fd00::2"}, Ports: []corev1.ServicePort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}}}}
}

func TestBootstrapServiceAdmission(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Service){
		"headless":    func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone },
		"pending":     func(s *corev1.Service) { s.Spec.ClusterIP = "" },
		"external":    func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName },
		"deleting":    func(s *corev1.Service) { now := metav1.Now(); s.DeletionTimestamp = &now },
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
				t.Fatal("invalid bootstrap Service admitted")
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
		o := originFixture()

		address, err := bootstrapAddress(o, "http", ip)
		if err != nil {
			t.Fatal(err)
		}

		want := "10.100.0.2:8080"
		if ip == "fd00::1" {
			want = "[fd00::2]:8080"
		}

		if address != want {
			t.Fatalf("bootstrap address = %s, want %s", address, want)
		}

		o.Spec.ClusterIPs = []string{"10.100.0.2"}
		if ip == "fd00::1" {
			if _, err := bootstrapAddress(o, "http", ip); err == nil {
				t.Fatal("missing bootstrap family accepted")
			}
		}
	}
}
