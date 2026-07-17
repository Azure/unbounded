// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCRDMaintainerNeedsLeaderElection(t *testing.T) {
	if !(&CRDMaintainer{}).NeedLeaderElection() {
		t.Fatal("CRDMaintainer must require leader election")
	}
}

func TestCRDMaintainerInvokesBootstrap(t *testing.T) {
	cli := fake.NewClientBuilder().Build()
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})

	maintainer := &CRDMaintainer{
		Client:   cli,
		Interval: time.Millisecond,
		Bootstrap: func(_ context.Context, got client.Client) error {
			if got != cli {
				t.Errorf("bootstrap client = %v, want direct client %v", got, cli)
			}

			close(called)
			cancel()

			return nil
		},
	}

	if err := maintainer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-called:
	default:
		t.Fatal("CRDMaintainer did not invoke Bootstrap")
	}
}

func TestCRDMaintainerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	maintainer := &CRDMaintainer{
		Interval: time.Hour,
		Bootstrap: func(context.Context, client.Client) error {
			called = true

			return nil
		},
	}

	if err := maintainer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if called {
		t.Fatal("Bootstrap called after cancellation")
	}
}

func TestCRDMaintainerKeepsRetryingOnPersistentFailureWithoutStopping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wantErr := errors.New("apply failed")
	calls := make(chan struct{}, 16)

	maintainer := &CRDMaintainer{
		Interval: time.Millisecond,
		Bootstrap: func(context.Context, client.Client) error {
			select {
			case calls <- struct{}{}:
			default:
			}

			return wantErr
		},
	}

	done := make(chan error, 1)

	go func() { done <- maintainer.Start(ctx) }()

	// A persistent failure must not stop the manager: established CRDs stay
	// served regardless, so the maintainer keeps retrying rather than returning.
	for i := 0; i < 4; i++ {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("maintainer stopped retrying after %d persistent failures", i)
		}
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Start on persistent failure = %v, want nil (context cancel is the only clean stop)", err)
	}
}

func TestCRDMaintainerRetriesTransientFailureWithoutCrashing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	transient := errors.New("transient apply failure")
	recovered := make(chan struct{})
	calls := 0

	maintainer := &CRDMaintainer{
		Interval: time.Millisecond,
		Bootstrap: func(context.Context, client.Client) error {
			calls++

			switch calls {
			case 1:
				return transient
			case 2:
				close(recovered)

				return nil
			default:
				return nil
			}
		},
	}

	done := make(chan error, 1)

	go func() { done <- maintainer.Start(ctx) }()

	select {
	case <-recovered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintainer did not recover from a transient failure")
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Start after transient failure = %v, want nil", err)
	}
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

	for _, name := range RequiredCRDNames {
		if !applied[name] {
			t.Fatalf("expected CRD %q to be installed by BootstrapCRDs (applied=%v)", name, applied)
		}
	}

	if len(applied) != len(RequiredCRDNames) {
		t.Fatalf("BootstrapCRDs applied %d CRDs, want %d (applied=%v)", len(applied), len(RequiredCRDNames), applied)
	}
}

// TestBootstrapCRDsWaitsForEstablished proves BootstrapCRDs blocks on the
// Established condition rather than returning as soon as a CRD exists.
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
	if gets <= len(RequiredCRDNames) {
		t.Fatalf("expected BootstrapCRDs to re-poll for establishment; got %d Gets for %d CRDs", gets, len(RequiredCRDNames))
	}
}

func TestWaitForCRDsEstablishedRejectsDeletingCRDImmediately(t *testing.T) {
	now := metav1.Now()
	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				crd := obj.(*apiextensionsv1.CustomResourceDefinition)
				crd.Name = key.Name
				crd.DeletionTimestamp = &now
				crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
					Type:   apiextensionsv1.Established,
					Status: apiextensionsv1.ConditionTrue,
				}}

				return nil
			},
		}).
		Build()

	started := time.Now()

	err := waitForCRDsEstablished(context.Background(), cli, []string{"deleting.example.com"})
	if err == nil || !strings.Contains(err.Error(), "deleting.example.com is being deleted") {
		t.Fatalf("waitForCRDsEstablished error = %v, want deleting CRD error", err)
	}

	if elapsed := time.Since(started); elapsed >= crdEstablishedPoll {
		t.Fatalf("waitForCRDsEstablished polled for %v instead of failing immediately", elapsed)
	}
}

func TestCRDEstablishedRejectsDeletingCRD(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type:   apiextensionsv1.Established,
				Status: apiextensionsv1.ConditionTrue,
			}},
		},
	}
	if !crdEstablished(crd) {
		t.Fatal("live Established CRD was rejected")
	}

	now := metav1.Now()

	crd.DeletionTimestamp = &now
	if crdEstablished(crd) {
		t.Fatal("terminating CRD was treated as established")
	}
}

func TestRequiredCRDNames(t *testing.T) {
	want := [...]string{
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

	if RequiredCRDNames != want {
		t.Fatalf("RequiredCRDNames = %v, want %v", RequiredCRDNames, want)
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
