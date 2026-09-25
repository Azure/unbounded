// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Azure/unbounded/internal/racer"
)

func main() {
	ctrl.SetLogger(zap.New())

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 2 || len(os.Args) == 2 && os.Args[1] != "initialize" {
		return fmt.Errorf("usage: racer-controller [initialize]")
	}

	cfg, err := racer.LoadConfig()
	if err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()
	if len(os.Args) == 2 {
		return racer.Initialize(ctx, cfg)
	}

	return racer.Run(ctx, cfg)
}
