// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package v1alpha1 contains API Schema definitions for the unboundednet v1alpha1 API group
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupName is the group name used in this package
const GroupName = "net.unbounded-kube.io"

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeGroupVersion is an alias for GroupVersion for backward compatibility.
	SchemeGroupVersion = GroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}

func init() {
	SchemeBuilder.Register(
		&Site{}, &SiteList{},
		&SiteNodeSlice{}, &SiteNodeSliceList{},
		&GatewayPool{}, &GatewayPoolList{},
		&GatewayPoolNode{}, &GatewayPoolNodeList{},
		&SitePeering{}, &SitePeeringList{},
		&SiteGatewayPoolAssignment{}, &SiteGatewayPoolAssignmentList{},
		&GatewayPoolPeering{}, &GatewayPoolPeeringList{},
	)
}
