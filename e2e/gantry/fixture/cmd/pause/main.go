// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// The fixture workload only needs to remain running after containerd unpacks it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
}
