// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package lib is a fixture for the build-tag handling in
// hack/cmd/unreferenced.
package lib

// OnlyUnderTag is called only from a file behind the `special` tag. Scanning
// without that tag reports it; scanning with it does not; scanning both must
// not, because a reference under any configuration is a reference.
func OnlyUnderTag() {}

// NeverUsed is dead under every configuration.
func NeverUsed() {}
