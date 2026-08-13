// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package main

import (
	"fmt"
	"os"
)

// main refuses to run off Linux.
//
// The snapshotter is built out of device mapper, overlayfs and EROFS, none of
// which exist elsewhere. The command still compiles on every platform so
// `go build ./...` and the linters work on a developer's machine, but it does
// not pretend it can serve containerd.
func main() {
	fmt.Fprintln(os.Stderr, "gantry-snapshotter requires linux")
	os.Exit(1)
}
