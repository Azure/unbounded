// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testProviderKubeProxyImage = "provider.example/kube-proxy:v1.34.2-1"
	testServerVersion          = "v1.34.2"
	testImageDigest            = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// kubeSystemKubeProxy is a provider kube-proxy DaemonSet whose only container
// is named and imaged as given.
func kubeSystemKubeProxy(container, image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: metav1.NamespaceSystem},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: container, Image: image}},
		}}},
	}
}

func clientsetWithServerVersion(gitVersion string, objects ...runtime.Object) *k8sfake.Clientset {
	clientset := k8sfake.NewClientset(objects...)
	clientset.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: gitVersion}

	return clientset
}

func TestManagedKubeProxyImage(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		provider   *appsv1.DaemonSet
		want       string
	}{
		{
			name:     "unset reuses the provider image",
			provider: kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:     testProviderKubeProxyImage,
		},
		{
			name: "unset without a provider falls back to registry.k8s.io",
			want: "registry.k8s.io/kube-proxy:" + testServerVersion,
		},
		{
			name:       "a repository takes the provider tag",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a repository without a provider takes the server version",
			configured: "myreg.example/kube-proxy",
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:       "a registry port is not mistaken for a tag",
			configured: "localhost:5000/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "localhost:5000/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a pinned tag is used as-is",
			configured: "myreg.example/kube-proxy:v1.34.0",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy:v1.34.0",
		},
		{
			name:       "a pinned digest is used as-is",
			configured: "myreg.example/kube-proxy@" + testImageDigest,
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy@" + testImageDigest,
		},
		{
			name:       "only the provider tag carries over, not its digest",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage+"@"+testImageDigest),
			want:       "myreg.example/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a provider image pinned only by digest gives the server version",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", "provider.example/kube-proxy@"+testImageDigest),
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:       "an unparsable provider image gives the server version",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", "Provider.example/Kube-Proxy:v1"),
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:     "a provider without a kube-proxy container counts as absent",
			provider: kubeSystemKubeProxy("proxy", testProviderKubeProxyImage),
			want:     "registry.k8s.io/kube-proxy:" + testServerVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.provider != nil {
				objects = append(objects, tt.provider)
			}

			got, err := ManagedKubeProxyImage(t.Context(), clientsetWithServerVersion(testServerVersion, objects...), tt.configured)
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if got != tt.want {
				t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestManagedKubeProxyImagePinnedReadsNothing pins that a configured version is
// final: nothing in the cluster can change it, so nothing is read.
func TestManagedKubeProxyImagePinnedReadsNothing(t *testing.T) {
	clientset := clientsetWithServerVersion(testServerVersion, kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))

	if _, err := ManagedKubeProxyImage(t.Context(), clientset, "myreg.example/kube-proxy:v1.34.0"); err != nil {
		t.Fatalf("ManagedKubeProxyImage() error = %v", err)
	}

	if actions := clientset.Actions(); len(actions) != 0 {
		t.Fatalf("a pinned image read the cluster: %v", actions)
	}
}

func TestManagedKubeProxyImageRejectsMalformedValues(t *testing.T) {
	for _, configured := range []string{"myreg.example/Kube-Proxy", "myreg.example/kube-proxy:", " myreg.example/kube-proxy"} {
		t.Run(configured, func(t *testing.T) {
			clientset := clientsetWithServerVersion(testServerVersion)

			if got, err := ManagedKubeProxyImage(t.Context(), clientset, configured); err == nil {
				t.Fatalf("ManagedKubeProxyImage(%q) = %q, want an error", configured, got)
			}

			if actions := clientset.Actions(); len(actions) != 0 {
				t.Fatalf("a malformed value read the cluster: %v", actions)
			}
		})
	}
}

// TestManagedKubeProxyImageFallsBackWhenTheClusterCannotBeRead keeps the
// behavior that predates repository values: a failed read degrades the choice
// of image rather than stopping managed kube-proxy.
func TestManagedKubeProxyImageFallsBackWhenTheClusterCannotBeRead(t *testing.T) {
	for _, tt := range []struct {
		configured string
		want       string
	}{
		{want: "registry.k8s.io/kube-proxy:latest"},
		{configured: "myreg.example/kube-proxy", want: "myreg.example/kube-proxy:latest"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			clientset := k8sfake.NewClientset(kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))
			unavailable := func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("apiserver unavailable")
			}

			clientset.PrependReactor("get", "daemonsets", unavailable)
			clientset.PrependReactor("get", "version", unavailable)

			got, err := ManagedKubeProxyImage(t.Context(), clientset, tt.configured)
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if got != tt.want {
				t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, tt.want)
			}
		})
	}
}
