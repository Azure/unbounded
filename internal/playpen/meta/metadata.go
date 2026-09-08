// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meta

const (
	AnnotationClientWireGuardPublicKey = "playpen.unbounded-cloud.io/client-wireguard-public-key"
	AnnotationServerWireGuardPublicKey = "playpen.unbounded-cloud.io/server-wireguard-public-key"
	AnnotationRedfishCertPEM           = "playpen.unbounded-cloud.io/redfish-cert-pem"
	AnnotationIdempotencyKeyHash       = "playpen.unbounded-cloud.io/idempotency-key-hash"
	AnnotationRequestHash              = "playpen.unbounded-cloud.io/request-hash"
	AnnotationClaimedAt                = "playpen.unbounded-cloud.io/claimed-at"
	AnnotationControlPlaneKubeconfig   = "playpen.unbounded-cloud.io/control-plane-kubeconfig"
	AnnotationControlPlaneGuestServer  = "playpen.unbounded-cloud.io/control-plane-guest-server"

	LabelAllocated         = "playpen.unbounded-cloud.io/allocated"
	LabelArchitecture      = "playpen.unbounded-cloud.io/architecture"
	LabelResourceType      = "playpen.unbounded-cloud.io/resource-type"
	LabelKubernetesVersion = "playpen.unbounded-cloud.io/kubernetes-version"

	ArchitectureAMD64 = "amd64"
	ArchitectureARM64 = "arm64"

	ResourceTypeRunner       = "runner"
	ResourceTypeControlPlane = "controlPlane"
)
