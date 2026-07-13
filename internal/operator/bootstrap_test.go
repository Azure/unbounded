// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// embeddedCRDNames are every CustomResourceDefinition the operator installs at
// startup: the six machina-group and six net-group CRDs embedded in the machina
// and net manifests. BootstrapCRDs must apply all of them so a cluster can be
// maintained by applying the operator manifests alone.
var embeddedCRDNames = []string{
	"machines.unbounded-cloud.io",
	"machineoperations.unbounded-cloud.io",
	"sites.unbounded-cloud.io",
	"machineconfigurations.unbounded-cloud.io",
	"machineoperationcredentials.unbounded-cloud.io",
	"machineconfigurationversions.unbounded-cloud.io",
	"sitenodeslices.net.unbounded-cloud.io",
	"gatewaypools.net.unbounded-cloud.io",
	"gatewaypoolnodes.net.unbounded-cloud.io",
	"sitegatewaypoolassignments.net.unbounded-cloud.io",
	"sitepeerings.net.unbounded-cloud.io",
	"gatewaypoolpeerings.net.unbounded-cloud.io",
}

func TestBootstrapCRDsTimeoutCancelsBlockedApply(t *testing.T) {
	applyStarted := make(chan struct{})

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				close(applyStarted)
				<-ctx.Done()

				return ctx.Err()
			},
		}).
		Build()

	err := bootstrapCRDs(context.Background(), cli, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bootstrapCRDs error = %v, want context deadline exceeded", err)
	}

	select {
	case <-applyStarted:
	default:
		t.Fatal("BootstrapCRDs did not attempt an apply")
	}
}

// TestBootstrapCRDsAppliesEmbeddedCRDs exercises the real startup CRD bootstrap
// against the embedded machina + net manifests (rendered by `make test`). A fake
// client records the applied CRDs and reports them Established so the wait
// completes. It guards that the operator installs every machina and net CRD
// itself, which is what lets a cluster be maintained by applying the operator
// manifests alone.
func TestBootstrapCRDsAppliesEmbeddedCRDs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apiextensions: %v", err)
	}

	applied := map[string]bool{}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				if named, ok := obj.(interface{ GetName() string }); ok {
					applied[named.GetName()] = true
				}

				return nil
			},
			// Report every CRD Established so waitForCRDsEstablished returns
			// promptly.
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
				if !ok {
					return apierrorsNotImplemented(key)
				}

				crd.Name = key.Name
				crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
					Type:   apiextensionsv1.Established,
					Status: apiextensionsv1.ConditionTrue,
				}}

				return nil
			},
		}).
		Build()

	if err := BootstrapCRDs(context.Background(), cli); err != nil {
		t.Fatalf("BootstrapCRDs: %v", err)
	}

	for _, name := range embeddedCRDNames {
		if !applied[name] {
			t.Fatalf("expected CRD %q to be installed by BootstrapCRDs (applied=%v)", name, applied)
		}
	}

	if len(applied) != len(embeddedCRDNames) {
		t.Fatalf("BootstrapCRDs applied %d CRDs, want %d (applied=%v)", len(applied), len(embeddedCRDNames), applied)
	}
}

// TestBootstrapCRDsWaitsForEstablished proves BootstrapCRDs blocks on the
// Established condition rather than returning as soon as a CRD exists: the fake
// client reports the CRDs not-Established on the first observation and
// Established thereafter, so BootstrapCRDs can only succeed by re-polling.
func TestBootstrapCRDsWaitsForEstablished(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apiextensions: %v", err)
	}

	var (
		mu        sync.Mutex
		gets      int
		firstSeen bool
	)

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return nil
			},
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
				if !ok {
					return apierrorsNotImplemented(key)
				}

				crd.Name = key.Name

				mu.Lock()
				gets++
				established := firstSeen
				firstSeen = true
				mu.Unlock()

				// First observation reports not-Established (no condition); every
				// later poll reports Established.
				if established {
					crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
						Type:   apiextensionsv1.Established,
						Status: apiextensionsv1.ConditionTrue,
					}}
				}

				return nil
			},
		}).
		Build()

	if err := BootstrapCRDs(context.Background(), cli); err != nil {
		t.Fatalf("BootstrapCRDs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// A single pass (one Get) would mean it never waited for establishment.
	if gets <= len(embeddedCRDNames) {
		t.Fatalf("expected BootstrapCRDs to re-poll for establishment; got %d Gets for %d CRDs", gets, len(embeddedCRDNames))
	}
}

// apierrorsNotImplemented is a small helper so a non-CRD Get in the test fails
// loudly rather than silently.
func apierrorsNotImplemented(key client.ObjectKey) error {
	return &notImplementedError{key: key}
}

type notImplementedError struct{ key client.ObjectKey }

func (e *notImplementedError) Error() string {
	return "unexpected Get for non-CRD object: " + e.key.String()
}
