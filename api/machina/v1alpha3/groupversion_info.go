// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=unbounded-cloud.io
package v1alpha3

//go:generate controller-gen object:headerFile=../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../deploy/machina/crd

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "unbounded-cloud.io", Version: "v1alpha3"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Resource returns a GroupResource for this API group.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
