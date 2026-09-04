// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=net.unbounded-cloud.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../deploy/net/crd
