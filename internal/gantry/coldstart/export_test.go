// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coldstart

import "github.com/Azure/unbounded/internal/gantry/ifaces"

// KindLabelForTest exposes the unexported kindLabel helper to the
// external _test package so the kind-to-Prometheus-label mapping
// can be unit-tested without making the helper part of the public
// API. The helper is intentionally unexported because it is purely
// internal to the metrics-naming convention; this shim keeps it
// that way while still allowing direct table-driven coverage.
func KindLabelForTest(k ifaces.OriginRefKind) string { return kindLabel(k) }
