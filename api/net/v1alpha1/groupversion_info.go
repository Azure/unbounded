// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// +kubebuilder:object:generate=true
// +groupName=net.unbounded-kube.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../hack/boilerplate.go.txt paths=.
//go:generate controller-gen crd:headerFile=../../../hack/boilerplate.yaml.txt paths=. output:crd:dir=../../../deploy/net/crds
