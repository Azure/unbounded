// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReapLegacyResourcesConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		setEnv  bool
		env     string
		args    []string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults true", want: true},
		{name: "valid false environment", setEnv: true, env: "false", want: false},
		{name: "valid true environment", setEnv: true, env: "true", want: true},
		{name: "empty environment errors", setEnv: true, env: "", wantErr: true},
		{name: "invalid environment errors", setEnv: true, env: "notabool", wantErr: true},
		{
			name:   "explicit flag overrides malformed environment",
			setEnv: true,
			env:    "notabool",
			args:   []string{"--reap-legacy-resources=false"},
			want:   false,
		},
	}

	const key = "UNBOUNDED_REAP_LEGACY_RESOURCES"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv(key, tc.env)
			} else {
				unsetenv(t, key)
			}

			called := false

			var got config

			cmd := newCommand(func(_ context.Context, cfg config) error {
				called = true
				got = cfg

				return nil
			})
			cmd.SetArgs(tc.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			err := cmd.Execute()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("Execute error = %v, want error naming %s", err, key)
				}

				if called {
					t.Fatal("runner called after environment parsing error")
				}

				return
			}

			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}

			if !called {
				t.Fatal("runner was not called")
			}

			if got.reapLegacyResources != tc.want {
				t.Fatalf("reapLegacyResources = %v, want %v", got.reapLegacyResources, tc.want)
			}
		})
	}
}

func TestImageRegistryConfiguration(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want string
	}{
		{name: "default", want: "ghcr.io"},
		{name: "environment", env: "mirror.example.com", want: "mirror.example.com"},
		{name: "flag overrides environment", env: "mirror.example.com", args: []string{"--image-registry=registry.example.com"}, want: "registry.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UNBOUNDED_IMAGE_REGISTRY", tc.env)

			var got config

			cmd := newCommand(func(_ context.Context, cfg config) error {
				got = cfg

				return nil
			})
			cmd.SetArgs(tc.args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if got.imageRegistry != tc.want {
				t.Fatalf("imageRegistry = %q, want %q", got.imageRegistry, tc.want)
			}
		})
	}
}

func TestResolveAPIServerEndpoint(t *testing.T) {
	const clusterInfoKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: dGVzdC1jYQ==
    server: https://discovered.example:6443
  name: ""
kind: Config
`

	clusterInfoCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespacePublic, Name: "cluster-info"},
		Data:       map[string]string{"kubeconfig": clusterInfoKubeconfig},
	}

	t.Run("override wins without discovery", func(t *testing.T) {
		client := fake.NewSimpleClientset() // no cluster-info; must not be consulted

		got, err := resolveAPIServerEndpoint(context.Background(), "https://override.example:6443", client)
		if err != nil {
			t.Fatalf("resolveAPIServerEndpoint: %v", err)
		}

		if got != "https://override.example:6443" {
			t.Fatalf("endpoint = %q, want override", got)
		}
	})

	t.Run("discovers from cluster-info when override empty", func(t *testing.T) {
		client := fake.NewSimpleClientset(clusterInfoCM)

		got, err := resolveAPIServerEndpoint(context.Background(), "", client)
		if err != nil {
			t.Fatalf("resolveAPIServerEndpoint: %v", err)
		}

		if got != "https://discovered.example:6443" {
			t.Fatalf("endpoint = %q, want discovered", got)
		}
	})

	t.Run("fails hard when empty override and no cluster-info", func(t *testing.T) {
		// No cluster-info and no external KUBERNETES_SERVICE_HOST FQDN.
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "")

		client := fake.NewSimpleClientset()
		if _, err := resolveAPIServerEndpoint(context.Background(), "", client); err == nil {
			t.Fatal("expected hard error when no override and cluster-info missing")
		}
	})

	t.Run("fails hard when only the in-cluster ClusterIP is available", func(t *testing.T) {
		// Without cluster-info, an in-cluster ClusterIP in KUBERNETES_SERVICE_HOST
		// must not be advertised (a joining node cannot reach it).
		t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
		t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")

		client := fake.NewSimpleClientset()
		if _, err := resolveAPIServerEndpoint(context.Background(), "", client); err == nil {
			t.Fatal("expected hard error when only the in-cluster ClusterIP is available")
		}
	})
}

func unsetenv(t *testing.T, key string) {
	t.Helper()

	value, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}

	t.Cleanup(func() {
		if ok {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}

			return
		}

		if err := os.Unsetenv(key); err != nil {
			t.Errorf("unset %s during cleanup: %v", key, err)
		}
	})
}
