// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package utilio

import "fmt"

// HostKernel is not supported on non-Linux platforms.
func HostKernel() (string, error) {
	return "", fmt.Errorf("HostKernel is only supported on Linux")
}
