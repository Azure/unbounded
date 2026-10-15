//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operatore2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator"
)

func createClusterNamed(t *testing.T, name string) string {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") == "1" {
			t.Logf("E2E_KEEP=1; leaving kind cluster %q", name)
			return
		}

		if err := run(context.Background(), "kind", "delete", "cluster", "--name", name); err != nil {
			t.Errorf("kind cleanup: %v", err)
		}
	})

	if err := run(t.Context(), "kind", "create", "cluster", "--name", name, "--wait", "120s", "--kubeconfig", kubeconfig); err != nil {
		t.Fatalf("kind create cluster: %v", err)
	}

	return kubeconfig
}

func applyCRDs(t *testing.T, kubeconfig string) {
	t.Helper()

	if err := operator.BootstrapCRDs(t.Context(), newClient(t, kubeconfig)); err != nil {
		t.Fatalf("bootstrap current CRDs: %v", err)
	}
}

func newClient(t *testing.T, kubeconfig string) client.Client {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build rest config: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	return cli
}

func mustCreate(ctx context.Context, t *testing.T, cli client.Client, obj client.Object) {
	t.Helper()

	if err := cli.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func requireBins(t *testing.T, bins ...string) {
	t.Helper()

	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("e2e prerequisite %q missing on PATH", bin)
		}
	}
}

func run(ctx context.Context, name string, args ...string) error {
	// GNU timeout sends TERM to the process group and cleans up children.
	argv := append([]string{"--signal=TERM", "--kill-after=10s", "180s", name}, args...)
	cmd := exec.CommandContext(ctx, "timeout", argv...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.WaitDelay = 10 * time.Second

	return cmd.Run()
}
