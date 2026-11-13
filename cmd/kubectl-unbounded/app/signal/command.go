// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/signalplan"
)

type renderOptions struct {
	filename string
}

func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "signal",
		Short: "Author Project Signal certification resources",
	}

	cmd.AddCommand(newRenderCommand())

	return cmd
}

func newRenderCommand() *cobra.Command {
	options := &renderOptions{}
	cmd := &cobra.Command{
		Use:   "render -f PLAN",
		Short: "Render CertificationTests from a source plan",
		Long: "Read a CertificationPlan, resolve local artifact files relative to its source root, " +
			"and emit deterministic CertificationTest YAML with embedded digest-verified content.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rendered, err := signalplan.CompileFile(options.filename)
			if err != nil {
				return err
			}

			if _, err := cmd.OutOrStdout().Write(rendered); err != nil {
				return fmt.Errorf("write rendered CertificationTests: %w", err)
			}

			return nil
		},
	}
	cmd.Flags().StringVarP(&options.filename, "filename", "f", "", "CertificationPlan YAML file")

	if err := cmd.MarkFlagRequired("filename"); err != nil {
		panic(err)
	}

	return cmd
}
