// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the NodeGrant API served by the rebound
// aggregated API server. NodeGrant is not a CRD: rebound stores grants in a
// ConfigMap ledger and serves them through an APIService.
//
// +kubebuilder:object:generate=true
// +k8s:openapi-gen=true
// +groupName=rebound.unbounded-cloud.io
package v1alpha1
