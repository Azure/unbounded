// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package cmd

import "fmt"

// hostKernel is not supported on non-Linux platforms.
func hostKernel() (string, error) {
	return "", fmt.Errorf("hostKernel is not supported on this platform")
}
