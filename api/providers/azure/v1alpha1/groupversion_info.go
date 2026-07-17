// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// +kubebuilder:object:generate=true
// +groupName=azure.unbounded-cloud.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../../deploy/machina/crd

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version for Azure provider resources.
	GroupVersion = schema.GroupVersion{Group: "azure.unbounded-cloud.io", Version: "v1alpha1"}

	// SchemeBuilder registers Azure provider API types.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds Azure provider API types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
