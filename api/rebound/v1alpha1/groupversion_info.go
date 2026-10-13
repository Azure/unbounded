// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

//go:generate go tool controller-gen object:headerFile=../../../hack/boilerplate.go.txt paths=.
//go:generate go tool openapi-gen --output-file zz_generated.openapi.go --output-dir . --output-pkg github.com/Azure/unbounded/api/rebound/v1alpha1 --go-header-file ../../../hack/boilerplate.go.txt --report-filename /dev/null k8s.io/apimachinery/pkg/apis/meta/v1 k8s.io/apimachinery/pkg/runtime k8s.io/apimachinery/pkg/version github.com/Azure/unbounded/api/rebound/v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the API group served by rebound.
	GroupName = "rebound.unbounded-cloud.io"
	// Resource is the plural resource name for NodeGrant.
	Resource = "nodegrants"
	// Kind is the kind name for NodeGrant.
	Kind = "NodeGrant"
)

// SchemeGroupVersion is the group version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// GroupVersion is an alias of SchemeGroupVersion kept for symmetry with other
// API packages in this repository.
var GroupVersion = SchemeGroupVersion

var (
	// SchemeBuilder registers the NodeGrant types.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the NodeGrant types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// GroupResource returns the NodeGrant GroupResource for the given resource name.
func GroupResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource(Resource).GroupResource()
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &NodeGrant{}, &NodeGrantList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)

	return nil
}
