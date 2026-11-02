// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"sigs.k8s.io/yaml"
)

const machinaCRDDir = "../../../deploy/machina/crd"

// crdValidator validates custom resources in process against a generated CRD
// the same way the API server does: CRD validation, structural pruning and
// defaulting, OpenAPI schema validation, list-type validation, and CEL rules
// including transition rules, with validation ratcheting on update.
type crdValidator struct {
	crd             *apiextensions.CustomResourceDefinition
	structural      *structuralschema.Structural
	schemaValidator apiservervalidation.SchemaValidator
	celValidator    *cel.Validator
}

// loadCRD reads a generated CRD file and converts it to the internal type
// after applying the API server's v1 defaults.
func loadCRD(t *testing.T, file string) (*apiextensionsv1.CustomResourceDefinition, *apiextensions.CustomResourceDefinition) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(machinaCRDDir, file))
	if err != nil {
		t.Fatalf("read CRD %s: %v", file, err)
	}

	var v1CRD apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &v1CRD); err != nil {
		t.Fatalf("parse CRD %s: %v", file, err)
	}

	defaulted := v1CRD.DeepCopy()
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(defaulted)

	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(defaulted, &internal, nil); err != nil {
		t.Fatalf("convert CRD %s: %v", file, err)
	}

	// The API server records the storage version on create.
	for _, version := range internal.Spec.Versions {
		if version.Storage {
			internal.Status.StoredVersions = append(internal.Status.StoredVersions, version.Name)
		}
	}

	return &v1CRD, &internal
}

// newCRDValidator builds a validator for the single version of a generated CRD.
// It fails the test if the API server would reject the CRD itself.
func newCRDValidator(t *testing.T, file string) *crdValidator {
	t.Helper()

	_, crd := loadCRD(t, file)
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) > 0 {
		t.Fatalf("CRD %s is invalid: %v", file, errs.ToAggregate())
	}

	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("CRD %s has %d versions, want 1", file, len(crd.Spec.Versions))
	}

	validation, err := apiextensions.GetSchemaForVersion(crd, crd.Spec.Versions[0].Name)
	if err != nil || validation == nil || validation.OpenAPIV3Schema == nil {
		t.Fatalf("CRD %s has no schema: %v", file, err)
	}

	openAPISchema := validation.OpenAPIV3Schema

	structural, err := structuralschema.NewStructural(openAPISchema)
	if err != nil {
		t.Fatalf("build structural schema for %s: %v", file, err)
	}

	schemaValidator, _, err := apiservervalidation.NewSchemaValidator(openAPISchema)
	if err != nil {
		t.Fatalf("build schema validator for %s: %v", file, err)
	}

	return &crdValidator{
		crd:             crd,
		structural:      structural,
		schemaValidator: schemaValidator,
		celValidator:    cel.NewValidator(structural, true, celconfig.PerCallLimit),
	}
}

// admit prunes and defaults obj in place, as the API server does when it
// decodes a request body.
func (v *crdValidator) admit(obj map[string]any) {
	structuralpruning.Prune(obj, v.structural, true)
	structuraldefaulting.Default(obj, v.structural)
}

// validateCreate admits obj in place and returns the errors a create would
// report.
func (v *crdValidator) validateCreate(obj map[string]any) field.ErrorList {
	v.admit(obj)

	errs := apiservervalidation.ValidateCustomResource(nil, obj, v.schemaValidator)
	errs = append(errs, structurallisttype.ValidateListSetsAndMaps(nil, v.structural, obj)...)

	if blocked, err := hasBlockingErr(errs); blocked {
		return append(errs, err)
	}

	celErrs, _ := v.celValidator.Validate(context.Background(), nil, v.structural, obj, nil, celconfig.RuntimeCELCostBudget)

	return append(errs, celErrs...)
}

// validateUpdate admits newObj in place and returns the errors an update from
// oldObj would report. oldObj must already be admitted, like a stored object.
func (v *crdValidator) validateUpdate(newObj, oldObj map[string]any) field.ErrorList {
	v.admit(newObj)

	correlated := common.NewCorrelatedObject(newObj, oldObj, &model.Structural{Structural: v.structural})

	errs := apiservervalidation.ValidateCustomResourceUpdate(nil, newObj, oldObj, v.schemaValidator, apiservervalidation.WithRatcheting(correlated))
	if oldErrs := structurallisttype.ValidateListSetsAndMaps(nil, v.structural, oldObj); len(oldErrs) == 0 {
		errs = append(errs, structurallisttype.ValidateListSetsAndMaps(nil, v.structural, newObj)...)
	}

	if blocked, err := hasBlockingErr(errs); blocked {
		return append(errs, err)
	}

	celErrs, _ := v.celValidator.Validate(context.Background(), nil, v.structural, newObj, oldObj, celconfig.RuntimeCELCostBudget, cel.WithRatcheting(correlated))

	return append(errs, celErrs...)
}

// hasBlockingErr matches the API server: CEL rules are skipped when schema
// validation already found errors that could make them fail to evaluate.
func hasBlockingErr(errs field.ErrorList) (bool, *field.Error) {
	for _, err := range errs {
		switch err.Type {
		case field.ErrorTypeNotSupported, field.ErrorTypeRequired, field.ErrorTypeTooLong, field.ErrorTypeTooMany, field.ErrorTypeTypeInvalid:
			return true, field.Invalid(nil, nil, "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation")
		}
	}

	return false, nil
}

// parseObject parses a YAML or JSON object the way the API server decodes a
// request body: integers become int64 and other numbers float64.
func parseObject(t *testing.T, text string) map[string]any {
	t.Helper()

	data, err := yaml.YAMLToJSON([]byte(text))
	if err != nil {
		t.Fatalf("convert object to JSON: %v", err)
	}

	var obj map[string]any
	if err := utiljson.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse object: %v", err)
	}

	return obj
}

// setField sets a nested field, failing the test if a parent is not an object.
func setField(t *testing.T, obj map[string]any, value any, path ...string) {
	t.Helper()

	if err := unstructured.SetNestedField(obj, value, path...); err != nil {
		t.Fatalf("set %s: %v", strings.Join(path, "."), err)
	}
}

// assertErrors fails unless errs is empty when want is empty, or every string
// in want appears in errs otherwise. Each want entry is a field path, a CEL
// message, or both.
func assertErrors(t *testing.T, errs field.ErrorList, want []string) {
	t.Helper()

	if len(want) == 0 {
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs.ToAggregate())
		}

		return
	}

	if len(errs) == 0 {
		t.Fatalf("got no errors, want %q", want)
	}

	got := errs.ToAggregate().Error()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("errors do not contain %q: %s", w, got)
		}
	}
}
