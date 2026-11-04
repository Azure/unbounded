// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import "io"

func closeQuietly(closer io.Closer) {
	_ = closer.Close() //nolint:errcheck // Read-handle and shutdown cleanup cannot recover from a close failure.
}
