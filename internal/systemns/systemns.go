// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package systemns defines the namespace Unbounded components run in.
// Components, rendered manifests and client tools must agree on it, or a
// component would look for its peers, Secrets and leases in the wrong place.
package systemns

import "os"

// Default is the namespace rendered manifests use and the default install
// namespace. Tools that retarget a rendered manifest to another namespace must
// rewrite this value, not Name(): Name follows the caller's own POD_NAMESPACE,
// so it would miss the namespace references written at build time. The Makefile
// UNBOUNDED_NAMESPACE variable and the manifest template defaults must match;
// this package's drift test checks them.
const Default = "unbounded-system"

// Name returns the namespace the caller runs in. Pods get POD_NAMESPACE through
// the Downward API, so a component follows the namespace it was deployed into.
// Client tools have no POD_NAMESPACE and get Default.
func Name() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}

	return Default
}
