// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package nodeupdate

import "fmt"

// hostKernel is a stub for non-Linux platforms.
func hostKernel() (string, error) {
	return "", fmt.Errorf("hostKernel is only supported on Linux")
}
