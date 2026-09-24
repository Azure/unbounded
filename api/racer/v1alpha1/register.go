// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 defines the Racer cache API.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "racer.unbounded-cloud.io"

var (
	GroupVersion  = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &ClusterCache{}, &ClusterCacheList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
	AddToScheme = SchemeBuilder.AddToScheme
)
