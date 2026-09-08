// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package clusterinfo

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// validKubeconfig is a minimal cluster-info kubeconfig with a server URL and a
// (dummy) CA certificate, matching the shape kubeadm publishes.
const validKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: dGVzdC1jYQ==
    server: https://control-plane.example:6443
  name: ""
contexts: null
current-context: ""
kind: Config
preferences: {}
users: null
`

func clusterInfoCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespacePublic, Name: "cluster-info"},
		Data:       data,
	}
}

func TestResolveReturnsURLAndCA(t *testing.T) {
	client := fake.NewSimpleClientset(clusterInfoCM(map[string]string{"kubeconfig": validKubeconfig}))

	info, err := Resolve(context.Background(), client)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if info.ApiserverURL != "https://control-plane.example:6443" {
		t.Fatalf("ApiserverURL = %q, want https://control-plane.example:6443", info.ApiserverURL)
	}

	if string(info.CACertPEM) != "test-ca" {
		t.Fatalf("CACertPEM = %q, want test-ca", info.CACertPEM)
	}
}

func TestResolveApiserverURL(t *testing.T) {
	client := fake.NewSimpleClientset(clusterInfoCM(map[string]string{"kubeconfig": validKubeconfig}))

	url, err := ResolveApiserverURL(context.Background(), client)
	if err != nil {
		t.Fatalf("ResolveApiserverURL: %v", err)
	}

	if url != "https://control-plane.example:6443" {
		t.Fatalf("url = %q, want https://control-plane.example:6443", url)
	}
}

func TestResolveErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects []*corev1.ConfigMap
	}{
		{
			name:    "missing configmap",
			objects: nil,
		},
		{
			name:    "missing kubeconfig key",
			objects: []*corev1.ConfigMap{clusterInfoCM(map[string]string{"other": "x"})},
		},
		{
			name:    "malformed kubeconfig",
			objects: []*corev1.ConfigMap{clusterInfoCM(map[string]string{"kubeconfig": "not: [valid"})},
		},
		{
			name: "no server url",
			objects: []*corev1.ConfigMap{clusterInfoCM(map[string]string{"kubeconfig": `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: dGVzdC1jYQ==
    server: ""
  name: ""
kind: Config
`})},
		},
		{
			name: "no ca",
			objects: []*corev1.ConfigMap{clusterInfoCM(map[string]string{"kubeconfig": `apiVersion: v1
clusters:
- cluster:
    server: https://control-plane.example:6443
  name: ""
kind: Config
`})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			for _, obj := range tc.objects {
				if _, err := client.CoreV1().ConfigMaps(obj.Namespace).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seed configmap: %v", err)
				}
			}

			if _, err := Resolve(context.Background(), client); err == nil {
				t.Fatal("Resolve: expected error, got nil")
			}
		})
	}
}
