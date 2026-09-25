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
	cfg, err := racer.LoadConfig()
	if err != nil {
		return err
	}

	return racer.Run(ctrl.SetupSignalHandler(), cfg)
}
