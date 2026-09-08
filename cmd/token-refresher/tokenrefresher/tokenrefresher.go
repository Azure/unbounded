// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package tokenrefresher

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/token-refresher/tokenrefresher/controller"
	"github.com/Azure/unbounded/internal/version"
)

func Run() {
	root := &cobra.Command{
		Use:   "token-refresher",
		Short: "Maintain bootstrap tokens for Unbounded sites",
	}

	root.AddCommand(controller.NewCommand())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
