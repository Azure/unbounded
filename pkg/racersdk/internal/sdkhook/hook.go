// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package sdkhook connects racersdktest to private SDK construction seams.
// It cannot import racersdk without creating an import cycle. The SDK assigns
// these hooks once in init; consumers import racersdk and assert the documented
// signatures of the any-typed hooks. Initialization precedes every use.
// Do not replace hooks, even in tests: other clients may be using them.
package sdkhook

var (
	// NewClientAt has type func(racersdk.ClientConfig, string) (*racersdk.Client, error).
	NewClientAt any
	// ServeOriginAt has type func(context.Context, racersdk.OriginConfig, racersdk.Origin, string) error.
	ServeOriginAt any
	// OriginDefaults has type func(racersdk.OriginConfig) (racersdk.OriginConfig, error).
	OriginDefaults any
	// InvalidOrigin has type func() error; it preserves the former fake's local error.
	InvalidOrigin func() error
)
