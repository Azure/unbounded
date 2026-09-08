// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package clusterinfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestKubeServiceHostEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    string
		portEnv map[string]string
		want    string
		wantOK  bool
	}{
		{
			name:    "external FQDN with https port",
			host:    "my-aks-dns-abcd1234.hcp.eastus.azmk8s.io",
			portEnv: map[string]string{"KUBERNETES_SERVICE_PORT_HTTPS": "443"},
			want:    "https://my-aks-dns-abcd1234.hcp.eastus.azmk8s.io:443",
			wantOK:  true,
		},
		{
			name:   "external FQDN defaults to 443 when no port env",
			host:   "control-plane.example.com",
			want:   "https://control-plane.example.com:443",
			wantOK: true,
		},
		{
			name:    "external FQDN falls back to KUBERNETES_SERVICE_PORT",
			host:    "control-plane.example.com",
			portEnv: map[string]string{"KUBERNETES_SERVICE_PORT": "6443"},
			want:    "https://control-plane.example.com:6443",
			wantOK:  true,
		},
		{
			name:   "empty host rejected",
			host:   "",
			wantOK: false,
		},
		{
			name:   "IPv4 ClusterIP rejected",
			host:   "10.0.0.1",
			wantOK: false,
		},
		{
			name:   "IPv6 ClusterIP rejected",
			host:   "fd00::1",
			wantOK: false,
		},
		{
			name:   "in-cluster service DNS rejected",
			host:   "kubernetes.default.svc",
			wantOK: false,
		},
		{
			name:   "in-cluster service FQDN rejected",
			host:   "kubernetes.default.svc.cluster.local",
			wantOK: false,
		},
		{
			name:   "bare kubernetes rejected",
			host:   "kubernetes",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Clear the port envs by default; set per-case below.
			t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "")
			t.Setenv("KUBERNETES_SERVICE_PORT", "")
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.host)

			for k, v := range tc.portEnv {
				t.Setenv(k, v)
			}

			got, ok := KubeServiceHostEndpoint()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}

			if ok && got != tc.want {
				t.Fatalf("endpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInClusterCA(t *testing.T) {
	t.Run("reads mounted CA", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(path, []byte("test-ca"), 0o600); err != nil {
			t.Fatalf("write CA: %v", err)
		}

		withInClusterCAPath(t, path)

		got, err := InClusterCA()
		if err != nil {
			t.Fatalf("InClusterCA: %v", err)
		}

		if string(got) != "test-ca" {
			t.Fatalf("CA = %q, want test-ca", got)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		withInClusterCAPath(t, filepath.Join(t.TempDir(), "absent.crt"))

		if _, err := InClusterCA(); err == nil {
			t.Fatal("InClusterCA: expected error for missing file, got nil")
		}
	})

	t.Run("empty file errors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.crt")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write CA: %v", err)
		}

		withInClusterCAPath(t, path)

		if _, err := InClusterCA(); err == nil {
			t.Fatal("InClusterCA: expected error for empty file, got nil")
		}
	})
}

func TestDiscoverURL(t *testing.T) {
	t.Run("cluster-info URL wins when present", func(t *testing.T) {
		client := fake.NewSimpleClientset(clusterInfoCM(map[string]string{"kubeconfig": validKubeconfig}))
		// A usable FQDN fallback exists too; cluster-info must take precedence.
		t.Setenv("KUBERNETES_SERVICE_HOST", "fallback.example.com")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")

		url, err := DiscoverURL(context.Background(), client)
		if err != nil {
			t.Fatalf("DiscoverURL: %v", err)
		}

		if url != "https://control-plane.example:6443" {
			t.Fatalf("url = %q, want cluster-info value", url)
		}
	})

	t.Run("falls back to FQDN when cluster-info absent", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "my-aks.hcp.eastus.azmk8s.io")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")

		url, err := DiscoverURL(context.Background(), fake.NewSimpleClientset())
		if err != nil {
			t.Fatalf("DiscoverURL: %v", err)
		}

		if url != "https://my-aks.hcp.eastus.azmk8s.io:443" {
			t.Fatalf("url = %q, want FQDN fallback", url)
		}
	})

	t.Run("succeeds without reading the in-cluster CA", func(t *testing.T) {
		// The URL path must never touch the CA: an unreadable CA mount must not
		// prevent endpoint resolution.
		withInClusterCAPath(t, filepath.Join(t.TempDir(), "absent.crt"))
		t.Setenv("KUBERNETES_SERVICE_HOST", "my-aks.hcp.eastus.azmk8s.io")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")

		if _, err := DiscoverURL(context.Background(), fake.NewSimpleClientset()); err != nil {
			t.Fatalf("DiscoverURL: %v", err)
		}
	})

	t.Run("errors when cluster-info absent and host is a ClusterIP", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")

		if _, err := DiscoverURL(context.Background(), fake.NewSimpleClientset()); err == nil {
			t.Fatal("DiscoverURL: expected error, got nil")
		}
	})
}

// withInClusterCAPath points the package-level CA path at a fixture for the
// duration of the test and restores it afterwards.
func withInClusterCAPath(t *testing.T, path string) {
	t.Helper()

	previous := inClusterCAPath
	inClusterCAPath = path

	t.Cleanup(func() { inClusterCAPath = previous })
}
