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
// omitted spec.components.gantry block to enabled=true. Gantry is the only
// opt-out component; without this default a Site that configures other
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
