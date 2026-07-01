// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package meta

const (
	AnnotationClientWireGuardPublicKey = "playpen.unbounded-cloud.io/client-wireguard-public-key"
	AnnotationServerWireGuardPublicKey = "playpen.unbounded-cloud.io/server-wireguard-public-key"
	AnnotationIdempotencyKeyHash       = "playpen.unbounded-cloud.io/idempotency-key-hash"
	AnnotationRequestHash              = "playpen.unbounded-cloud.io/request-hash"
	AnnotationClaimedAt                = "playpen.unbounded-cloud.io/claimed-at"

	LabelAllocated = "playpen.unbounded-cloud.io/allocated"
)
