// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/gantry/ifaces"

// Registry supplies ordinary pulls, authoritative GET metadata, and bounded
// ranges for Gantry's Racer mirror and its node-local origin.
type Registry interface {
	ifaces.OriginPuller
	ifaces.OriginMetadataPuller
	ifaces.OriginRangePuller
}
