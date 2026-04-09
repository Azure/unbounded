// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machina

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/machina/machina/controller"
	"github.com/Azure/unbounded-kube/internal/version"
)

func Run() {
	root := &cobra.Command{
		Use:   "machina",
		Short: "machina machine controller",
	}

	root.AddCommand(controller.NewCommand())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
