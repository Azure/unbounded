// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package goalstates

import "fmt"

// hostKernel is not supported on non-Linux platforms.
func hostKernel() (string, error) {
	return "", fmt.Errorf("hostKernel is only supported on Linux")
}
