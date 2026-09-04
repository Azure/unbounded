// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"encoding/base64"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testClusterInfoKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Y2x1c3Rlci1pbmZvLWNh
    server: https://control-plane.example:6443
  name: ""
kind: Config
`

func clusterInfoConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespacePublic, Name: "cluster-info"},
		Data:       map[string]string{"kubeconfig": testClusterInfoKubeconfig},
	}
}

func newTestWatcher(clientset *fake.Clientset, override string, caReader func() ([]byte, error)) *ClusterInfoWatcher {
	return &ClusterInfoWatcher{
		clientset:            clientset,
		log:                  slog.Default(),
		apiserverURLOverride: override,
		inClusterCA:          caReader,
	}
}

func failCAReader(t *testing.T) func() ([]byte, error) {
	t.Helper()

	return func() ([]byte, error) {
		t.Fatal("in-cluster CA reader should not be called when cluster-info supplies a CA")
		return nil, nil
	}
}

func TestClusterInfoWatcherRefresh(t *testing.T) {
	t.Run("uses cluster-info URL and CA", func(t *testing.T) {
		w := newTestWatcher(fake.NewSimpleClientset(clusterInfoConfigMap()), "", failCAReader(t))

		if err := w.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		info := w.ClusterInfo()
		if info.ApiserverURL != "https://control-plane.example:6443" {
			t.Fatalf("ApiserverURL = %q, want cluster-info value", info.ApiserverURL)
		}

		if decodeCA(t, info.CACertBase64) != "cluster-info-ca" {
			t.Fatalf("CA = %q, want cluster-info-ca", decodeCA(t, info.CACertBase64))
		}
	})

	t.Run("override URL wins, cluster-info CA retained", func(t *testing.T) {
		w := newTestWatcher(fake.NewSimpleClientset(clusterInfoConfigMap()), "https://override.example:6443", failCAReader(t))

		if err := w.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		info := w.ClusterInfo()
		if info.ApiserverURL != "https://override.example:6443" {
			t.Fatalf("ApiserverURL = %q, want override", info.ApiserverURL)
		}

		if decodeCA(t, info.CACertBase64) != "cluster-info-ca" {
			t.Fatalf("CA = %q, want cluster-info-ca", decodeCA(t, info.CACertBase64))
		}
	})

	t.Run("AKS: no cluster-info, override URL + in-cluster CA", func(t *testing.T) {
		w := newTestWatcher(fake.NewSimpleClientset(), "https://my-aks.hcp.eastus.azmk8s.io:443",
			func() ([]byte, error) { return []byte("in-cluster-ca"), nil })

		if err := w.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		info := w.ClusterInfo()
		if info.ApiserverURL != "https://my-aks.hcp.eastus.azmk8s.io:443" {
			t.Fatalf("ApiserverURL = %q, want override", info.ApiserverURL)
		}

		if decodeCA(t, info.CACertBase64) != "in-cluster-ca" {
			t.Fatalf("CA = %q, want in-cluster-ca", decodeCA(t, info.CACertBase64))
		}
	})

	t.Run("errors when no URL is available", func(t *testing.T) {
		w := newTestWatcher(fake.NewSimpleClientset(), "",
			func() ([]byte, error) { return []byte("in-cluster-ca"), nil })

		if err := w.refresh(context.Background()); err == nil {
			t.Fatal("refresh: expected error when no URL is available")
		}
	})

	t.Run("errors when no CA is available", func(t *testing.T) {
		w := newTestWatcher(fake.NewSimpleClientset(), "https://my-aks.hcp.eastus.azmk8s.io:443",
			func() ([]byte, error) { return nil, context.DeadlineExceeded })

		if err := w.refresh(context.Background()); err == nil {
			t.Fatal("refresh: expected error when no CA is available")
		}
	})
}

func decodeCA(t *testing.T, encoded string) string {
	t.Helper()

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode CA: %v", err)
	}

	return string(raw)
}
