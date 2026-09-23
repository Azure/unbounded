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
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func TestSiteCacheSizeQuantityJSON(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{`{}`, ""},
		{`{"cacheSize":null}`, ""},
		{`{"cacheSize":"2Ti"}`, "2Ti"},
		{`{"cacheSize":2199023255552}`, "2Ti"},
		{`{"cacheSize":"32.5Mi"}`, "34078720"},
		// Zero must remain explicit so resolution can reject it, not inherit.
		{`{"cacheSize":"0"}`, "0"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			var spec RacerComponentSpec
			if err := json.Unmarshal([]byte(tc.value), &spec); err != nil {
				t.Fatal(err)
			}

			if tc.want == "" {
				if spec.CacheSize != nil {
					t.Fatal("absent cacheSize must remain nil")
				}
			} else if spec.CacheSize == nil || spec.CacheSize.Cmp(resource.MustParse(tc.want)) != 0 {
				t.Fatalf("cacheSize = %v, want %s", spec.CacheSize, tc.want)
			}

			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}

			if _, present := fields["cacheSize"]; present != (tc.want != "") {
				t.Fatalf("cacheSize presence changed on round trip: %s", data)
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

	racer := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["components"].Properties["racer"]

	size, ok := racer.Properties["cacheSize"]
	if !ok || !size.XIntOrString || size.Pattern == "" {
		t.Fatal("cacheSize must have the Kubernetes quantity schema")
	}

	if size.Default != nil {
		t.Fatal("cacheSize omission must be preserved for runtime inheritance")
	}

	for _, required := range racer.Required {
		if required == "cacheSize" {
			t.Fatal("cacheSize must remain optional")
		}
	}

	assertSchemaValidations(t, size, map[string]string{
		"isQuantity(string(self)) && quantity(string(self)).compareTo(quantity('512Mi')) >= 0 && quantity(string(self)).compareTo(quantity('8589934591.9375Gi')) <= 0": "cacheSize must be a quantity between 512Mi and 8589934591.9375Gi",
	})

	var internalSchema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&size, &internalSchema, nil); err != nil {
		t.Fatal(err)
	}

	structural, err := schema.NewStructural(&internalSchema)
	if err != nil {
		t.Fatal(err)
	}

	validator := cel.NewValidator(structural, false, 1000000)
	if validator == nil {
		t.Fatal("missing cacheSize CEL validator")
	}

	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"minimum", "512Mi", true},
		{"unaligned", "512.5Mi", true},
		{"default", "10Gi", true},
		{"large", "3Ti", true},
		{"large decimal", "2.5Ti", true},
		{"integer JSON", int64(2199023255552), true},
		{"maximum", "9223372036787666944", true},
		{"maximum Gi", "8589934591.9375Gi", true},
		{"empty", "", false},
		{"invalid unit", "10GiB", false},
		{"zero", "0", false},
		{"negative", "-32Mi", false},
		{"below minimum", "536870911", false},
		{"alignment overflow", "9223372036787666945", false},
		{"Gi overflow", "8589934592Gi", false},
		{"binary overflow", "8Ei", false},
		{"decimal overflow", "1e100", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs, _ := validator.Validate(t.Context(), field.NewPath("cacheSize"), structural, tc.value, nil, 1000000)
			if (len(errs) == 0) != tc.valid {
				t.Fatalf("CEL validation of %v = %v, want valid=%v", tc.value, errs, tc.valid)
			}
		})
	}
}
