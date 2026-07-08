// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	"github.com/Azure/unbounded/internal/metalman/netbootinit"
)

func main() {
	installer := netbootinit.NewInstaller()
	if err := installer.Run(context.Background()); err != nil {
		installer.Fatal(err)
	}
}
