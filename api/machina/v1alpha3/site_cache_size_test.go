// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"encoding/json"
	"os"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"sigs.k8s.io/yaml"
)

func TestSiteCacheSizeQuantityJSON(t *testing.T) {
	for _, tc := range []struct {
		value string
	}{
		{`{}`},
		{`{"cacheSize":null}`},
		{`{"cacheSize":"2Ti"}`},
		{`{"cacheSize":2199023255552}`},
		{`{"cacheSize":"32.5Mi"}`},
		{`{"cacheSize":"0"}`},
	} {
		t.Run(tc.value, func(t *testing.T) {
			// Legacy Racer configuration has no typed Site API representation.
			var spec SiteComponents
			if err := json.Unmarshal([]byte(`{"racer":`+tc.value+`}`), &spec); err != nil {
				t.Fatal(err)
			}

			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}

			if _, present := fields["racer"]; present {
				t.Fatalf("removed Racer configuration survived round trip: %s", data)
			}
		})
	}
}

func TestSiteCacheSizeSchema(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_sites.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	components := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["components"]
	if _, present := components.Properties["racer"]; present {
		t.Fatal("Site schema still exposes Racer configuration")
	}

	var internalSchema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&components, &internalSchema, nil); err != nil {
		t.Fatal(err)
	}

	structural, err := schema.NewStructural(&internalSchema)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		value any
	}{
		{"minimum", "512Mi"},
		{"unaligned", "512.5Mi"},
		{"default", "10Gi"},
		{"large", "3Ti"},
		{"large decimal", "2.5Ti"},
		{"integer JSON", int64(2199023255552)},
		{"maximum", "9223372036787666944"},
		{"maximum Gi", "8589934591.9375Gi"},
		{"empty", ""},
		{"invalid unit", "10GiB"},
		{"zero", "0"},
		{"negative", "-32Mi"},
		{"below minimum", "536870911"},
		{"alignment overflow", "9223372036787666945"},
		{"Gi overflow", "8589934592Gi"},
		{"binary overflow", "8Ei"},
		{"decimal overflow", "1e100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := map[string]any{"racer": map[string]any{"enabled": true, "cacheSize": tc.value}, "gantry": map[string]any{"enabled": false}}
			pruning.Prune(object, structural, false)

			if _, present := object["racer"]; present {
				t.Fatalf("legacy Racer configuration was not pruned: %v", object)
			}

			if _, present := object["gantry"]; !present {
				t.Fatal("pruning removed supported Site configuration")
			}
		})
	}
}
