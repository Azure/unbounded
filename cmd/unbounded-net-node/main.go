// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Azure/unbounded/internal/net/nodeagent"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cmd := nodeagent.NewCommand()
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
