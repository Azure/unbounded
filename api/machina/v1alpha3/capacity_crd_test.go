// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"context"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCapacityCRDs(t *testing.T) {
	t.Parallel()

	type column struct{ name, typ, jsonPath string }

	tests := []struct {
		file      string
		shortName string
		columns   []column
	}{
		{
			file:      "unbounded-cloud.io_machineclasses.yaml",
			shortName: "mcls",
			columns: []column{
				{"Site", "string", ".spec.siteRef.name"},
				{"Provider", "string", ".spec.provider"},
				{"Ready", "string", ".status.conditions[?(@.type=='Ready')].status"},
				{"Age", "date", ".metadata.creationTimestamp"},
			},
		},
		{
			file:      "unbounded-cloud.io_machineallocations.yaml",
			shortName: "malloc",
			columns: []column{
				{"Class", "string", ".spec.classRef.name"},
				{"Lifecycle", "string", ".spec.lifecycle"},
				{"Allocated", "string", ".status.conditions[?(@.type=='Allocated')].status"},
				{"Node", "string", ".status.nodeRef.name"},
				{"Age", "date", ".metadata.creationTimestamp"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.shortName, func(t *testing.T) {
			t.Parallel()

			crd, internal := loadCRD(t, tt.file)
			if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), internal); len(errs) > 0 {
				t.Fatalf("ValidateCustomResourceDefinition: %v", errs.ToAggregate())
			}

			if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
				t.Errorf("scope = %q, want Cluster", crd.Spec.Scope)
			}

			if !slices.Equal(crd.Spec.Names.ShortNames, []string{tt.shortName}) {
				t.Errorf("shortNames = %v, want [%s]", crd.Spec.Names.ShortNames, tt.shortName)
			}

			if len(crd.Spec.Versions) != 1 {
				t.Fatalf("versions = %d, want 1", len(crd.Spec.Versions))
			}

			version := crd.Spec.Versions[0]
			if version.Name != "v1alpha3" || !version.Served || !version.Storage {
				t.Errorf("version = %s served=%t storage=%t, want v1alpha3 served and storage", version.Name, version.Served, version.Storage)
			}

			if version.Subresources == nil || version.Subresources.Status == nil {
				t.Error("status subresource is missing")
			}

			var columns []column
			for _, c := range version.AdditionalPrinterColumns {
				columns = append(columns, column{c.Name, c.Type, c.JSONPath})
			}

			if !slices.Equal(columns, tt.columns) {
				t.Errorf("printer columns = %v, want %v", columns, tt.columns)
			}

			root := version.Schema.OpenAPIV3Schema
			if !slices.Contains(root.Required, "spec") {
				t.Errorf("root required = %v, want it to contain spec", root.Required)
			}
		})
	}

	t.Run("allocation lifecycle default", func(t *testing.T) {
		t.Parallel()

		crd, _ := loadCRD(t, "unbounded-cloud.io_machineallocations.yaml")

		lifecycle := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["lifecycle"]
		if lifecycle.Default == nil || string(lifecycle.Default.Raw) != `"Active"` {
			t.Fatalf("spec.lifecycle default = %v, want \"Active\"", lifecycle.Default)
		}
	})

	t.Run("class providerConfigRef shape", func(t *testing.T) {
		t.Parallel()

		crd, _ := loadCRD(t, "unbounded-cloud.io_machineclasses.yaml")

		ref := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["providerConfigRef"]

		var properties []string
		for name := range ref.Properties {
			properties = append(properties, name)
		}

		slices.Sort(properties)

		if want := []string{"apiGroup", "kind", "name", "namespace"}; !slices.Equal(properties, want) {
			t.Errorf("providerConfigRef properties = %v, want %v", properties, want)
		}

		required := slices.Clone(ref.Required)
		slices.Sort(required)

		if want := []string{"kind", "name"}; !slices.Equal(required, want) {
			t.Errorf("providerConfigRef required = %v, want %v", required, want)
		}
	})
}

func TestCapacityTypesRegistered(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"MachineClass", "MachineClassList", "MachineAllocation", "MachineAllocationList"} {
		if _, err := scheme.New(GroupVersion.WithKind(kind)); err != nil {
			t.Errorf("scheme.New(%s): %v", kind, err)
		}
	}
}
