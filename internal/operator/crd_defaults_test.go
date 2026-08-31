// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

// TestSiteCRDDefaultsGantryEnabled guards that the generated Site CRD defaults an
// omitted spec.components.gantry block to enabled=true. Without this default a Site that configures other
// components but omits gantry would report a blank Gantry print column despite
// gantry being active. The default is emitted from the +kubebuilder:default
// marker on the Gantry field, so this catches a regression if that marker or the
// regeneration is dropped.
func TestSiteCRDDefaultsGantryEnabled(t *testing.T) {
	crd := findSiteCRD(t)

	var schema *apiextensionsv1.JSONSchemaProps

	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Schema != nil && crd.Spec.Versions[i].Schema.OpenAPIV3Schema != nil {
			schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}

	if schema == nil {
		t.Fatal("Site CRD has no version schema")
	}

	gantry := nestedSchemaProp(t, schema, "spec", "components", "gantry")
	if gantry.Default == nil {
		t.Fatal("spec.components.gantry has no default; the Gantry print column would be blank for gantry-enabled sites")
	}

	if got := string(gantry.Default.Raw); !strings.Contains(got, `"enabled":true`) {
		t.Fatalf("gantry default = %s, want it to set enabled=true", got)
	}
}

func TestSiteCRDDefaultsTokenRefresherEnabled(t *testing.T) {
	crd := findSiteCRD(t)

	var schema *apiextensionsv1.JSONSchemaProps

	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Schema != nil && crd.Spec.Versions[i].Schema.OpenAPIV3Schema != nil {
			schema = crd.Spec.Versions[i].Schema.OpenAPIV3Schema
			break
		}
	}

	if schema == nil {
		t.Fatal("Site CRD has no version schema")
	}

	refresher := nestedSchemaProp(t, schema, "spec", "components", "tokenRefresher")
	if refresher.Default == nil {
		t.Fatal("spec.components.tokenRefresher has no default")
	}

	if got := string(refresher.Default.Raw); !strings.Contains(got, `"enabled":true`) {
		t.Fatalf("tokenRefresher default = %s, want it to set enabled=true", got)
	}
}

// nestedSchemaProp walks Properties down the given path, failing the test if any
// segment is missing.
func nestedSchemaProp(t *testing.T, schema *apiextensionsv1.JSONSchemaProps, path ...string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()

	cur := schema

	for _, key := range path {
		next, ok := cur.Properties[key]
		if !ok {
			t.Fatalf("schema path %v: missing property %q", path, key)
		}

		cur = &next
	}

	return cur
}

// findSiteCRD extracts the sites CustomResourceDefinition from the embedded
// machina manifests (populated by `make machina-manifests`).
func findSiteCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	files, err := component.YamlFiles(machinamanifests.Manifests)
	if err != nil {
		t.Fatalf("list machina manifests: %v", err)
	}

	for _, file := range files {
		data, err := fs.ReadFile(machinamanifests.Manifests, file)
		if err != nil {
			t.Fatalf("read machina manifest %s: %v", file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				t.Fatalf("decode machina manifest %s: %v", file, err)
			}

			if obj.Object == nil || obj.GetKind() != component.CRDKind || obj.GetName() != "sites.unbounded-cloud.io" {
				continue
			}

			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, crd); err != nil {
				t.Fatalf("convert sites CRD: %v", err)
			}

			return crd
		}
	}

	t.Fatal("sites.unbounded-cloud.io CRD not found in embedded machina manifests (run `make machina-manifests`)")

	return nil
}

// TestSiteMustBeClusterScopedForTheScopedCache pins the invariant that the
// operator's cache scoping silently depends on.
//
// The manager's cache is scoped to the operator's own namespace
// (cmd/unbounded-operator/main.go), because otherwise it runs informers over
// every ConfigMap, Deployment and DaemonSet in the cluster to read the handful
// that live in one namespace.
//
// component.Env.ListSites performs an unscoped List, and it has to: every pass
// fans out over the result and every component decides whether it is enabled by
// inspecting it. Under a scoped cache, controller-runtime routes that List by
// the kind's scope. A cluster-scoped kind goes to a separate cluster-wide cache
// and returns everything. A namespaced kind returns only the scoped namespaces,
// and crucially returns no error while doing it (see
// multi_namespace_cache.go: the NamespaceAll path unions namespaceToCache
// rather than failing).
//
// So if Site ever became namespaced, ListSites would quietly return only the
// Sites in the operator's namespace. Every other Site would lose its per-Site
// metalman and storage workloads, the cluster components would conclude they
// were disabled, and nothing anywhere would report a problem. That is a
// cluster-wide outage produced by a one-line marker change in another package,
// which is why it is asserted here rather than left to a code comment.
func TestSiteMustBeClusterScopedForTheScopedCache(t *testing.T) {
	crd := findSiteCRD(t)

	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatalf("sites.unbounded-cloud.io is %q, want %q.\n\n"+
			"The operator's manager cache is scoped to a single namespace, and "+
			"component.Env.ListSites lists Sites without a namespace. A namespaced Site "+
			"makes that list silently return only the Sites in the operator's namespace, "+
			"with no error, and every other Site loses its per-Site workloads.\n\n"+
			"If Site must become namespaced, ListSites and the cache scoping have to change together.",
			crd.Spec.Scope, apiextensionsv1.ClusterScoped)
	}
}
