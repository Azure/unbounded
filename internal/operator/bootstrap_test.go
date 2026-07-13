// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestBootstrapCRDsAppliesEmbeddedCRDs exercises the real startup CRD bootstrap
// against the embedded machina + net manifests (rendered by `make test`). A fake
// client records the applied CRDs and reports them Established so the wait
// completes. It guards that the operator installs the Site and net CRDs itself,
// which is what lets a cluster be maintained by applying the operator manifests
// alone.
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

	for _, name := range []string{
		"sites.unbounded-cloud.io",
		"machines.unbounded-cloud.io",
		"gatewaypools.net.unbounded-cloud.io",
		"sitenodeslices.net.unbounded-cloud.io",
	} {
		if !applied[name] {
			t.Fatalf("expected CRD %q to be installed by BootstrapCRDs (applied=%v)", name, applied)
		}
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
