// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// +kubebuilder:object:generate=true
// +groupName=infrastructure.unbounded-cloud.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../../deploy/machina/crd

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version for provider infrastructure
	// resources shipped by Unbounded.
	GroupVersion = schema.GroupVersion{Group: "infrastructure.unbounded-cloud.io", Version: "v1alpha1"}

	// SchemeBuilder registers infrastructure API types.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds infrastructure API types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
