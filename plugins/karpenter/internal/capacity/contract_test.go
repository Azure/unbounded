// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package capacity_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

func TestProjectionMatchesGeneratedCapacitySchema(t *testing.T) {
	classes := generatedSchema(t, "machineclasses")
	allocations := generatedSchema(t, "machineallocations")
	classStatus := classes.Properties["status"]
	offering := classStatus.Properties["offerings"]
	allocationSpec := allocations.Properties["spec"]
	allocationStatus := allocations.Properties["status"]

	checkFields(t, reflect.TypeFor[nodeclass.MachineClassSpec](), classes.Properties["spec"], "class.spec")
	checkFields(t, reflect.TypeFor[nodeclass.MachineClassStatus](), classStatus, "class.status")
	checkFields(t, reflect.TypeFor[capacity.Offering](), *offering.Items.Schema, "class.status.offerings[]")
	checkFields(t, reflect.TypeFor[capacity.AllocationSpec](), allocationSpec, "allocation.spec")
	checkFields(t, reflect.TypeFor[capacity.AllocationStatus](), allocationStatus, "allocation.status")

	if _, found := classes.Properties["spec"].Properties["providerConfigRef"]; !found {
		t.Fatal("shared API must retain its opaque providerConfigRef")
	}

	// The lifecycle values the plugin writes must be the ones the schema accepts.
	var lifecycles []string
	for _, value := range allocationSpec.Properties["lifecycle"].Enum {
		lifecycles = append(lifecycles, strings.Trim(string(value.Raw), `"`))
	}

	for _, value := range []string{capacity.Active, capacity.Released} {
		if !slices.Contains(lifecycles, value) {
			t.Fatalf("lifecycle %q is not in the generated schema's %v", value, lifecycles)
		}
	}
}

func generatedSchema(t *testing.T, resource string) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	path := filepath.Join("../../../../deploy/machina/crd", "unbounded-cloud.io_"+resource+".yaml")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	crd := new(apiextensionsv1.CustomResourceDefinition)
	if err := yaml.UnmarshalStrict(content, crd); err != nil {
		t.Fatal(err)
	}

	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatal("capacity resources must be cluster scoped")
	}

	for _, version := range crd.Spec.Versions {
		if version.Name == capacity.AllocationGVK.Version {
			if version.Subresources == nil || version.Subresources.Status == nil {
				t.Fatal("capacity status must have its own subresource")
			}

			return *version.Schema.OpenAPIV3Schema
		}
	}

	t.Fatal("capacity wire version is absent")

	return apiextensionsv1.JSONSchemaProps{}
}

func checkFields(t *testing.T, projected reflect.Type, schema apiextensionsv1.JSONSchemaProps, path string) {
	t.Helper()

	if projected.Kind() == reflect.Pointer {
		checkFields(t, projected.Elem(), schema, path)

		return
	}

	if projected.Kind() == reflect.Slice {
		if schema.Type != "array" || schema.Items == nil || schema.Items.Schema == nil {
			t.Fatalf("%s is not a schema array", path)
		}

		checkFields(t, projected.Elem(), *schema.Items.Schema, path+"[]")

		return
	}

	if projected.Kind() != reflect.Struct {
		checkScalar(t, projected, schema, path)

		return
	}

	if projected.PkgPath() != reflect.TypeFor[capacity.AllocationSpec]().PkgPath() &&
		projected.PkgPath() != reflect.TypeFor[nodeclass.MachineClassStatus]().PkgPath() {
		return
	}

	for field := range projected.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		property, found := schema.Properties[name]
		if !found {
			t.Fatalf("%s.%s is absent from the generated capacity schema", path, name)
		}

		checkFields(t, field.Type, property, path+"."+name)
	}
}

func checkScalar(t *testing.T, projected reflect.Type, schema apiextensionsv1.JSONSchemaProps, path string) {
	t.Helper()

	var expected string

	switch projected.Kind() {
	case reflect.String:
		expected = "string"
	case reflect.Int, reflect.Int32, reflect.Int64:
		expected = "integer"
	case reflect.Map:
		expected = "object"
	default:
		t.Fatalf("%s has an unchecked projection type %s", path, projected)
	}

	if schema.Type != expected {
		t.Fatalf("%s is %s in code but %s in the generated schema", path, expected, schema.Type)
	}
}
