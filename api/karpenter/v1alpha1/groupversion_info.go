// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=karpenter.unbounded-cloud.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../deploy/karpenter-provider/crd

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "karpenter.unbounded-cloud.io", Version: "v1alpha1"}
	SchemeBuilder = &runtime.SchemeBuilder{}
	AddToScheme   = SchemeBuilder.AddToScheme
)
