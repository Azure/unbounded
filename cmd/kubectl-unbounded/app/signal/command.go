// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/Azure/unbounded/internal/signalplan"
)

type renderOptions struct {
	filename  string
	bindings  string
	inputs    string
	testsOnly bool
}

func Command() *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true)
	cmd := &cobra.Command{
		Use:   "signal",
		Short: "Author Project Signal certification resources",
	}

	cmd.AddCommand(newRenderCommand())
	cmd.AddCommand(newBindingsCommand(configFlags))

	return cmd
}

func newRenderCommand() *cobra.Command {
	options := &renderOptions{}
	cmd := &cobra.Command{
		Use:   "render -f PLAN",
		Short: "Render CertificationTests from a source plan",
		Long: "Read a CertificationPlan, resolve local artifact files relative to its source root, " +
			"and emit deterministic Signal CRDs with embedded digest-verified content. " +
			"Plans with a Suite use --tests-only for bootstrap and --bindings after Test approval.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if options.inputs != "" && options.bindings == "" {
				return fmt.Errorf("--inputs requires --bindings")
			}

			var (
				rendered []byte
				err      error
			)

			switch {
			case options.testsOnly:
				rendered, err = signalplan.CompileTestsFile(options.filename)
			case options.bindings != "":
				if options.inputs != "" {
					rendered, err = signalplan.CompileFileWithBindingsAndInputs(
						options.filename,
						options.bindings,
						options.inputs,
					)
				} else {
					rendered, err = signalplan.CompileFileWithBindings(
						options.filename,
						options.bindings,
					)
				}
			default:
				rendered, err = signalplan.CompileFile(options.filename)
			}

			if err != nil {
				return err
			}

			if _, err := cmd.OutOrStdout().Write(rendered); err != nil {
				return fmt.Errorf("write rendered Signal resources: %w", err)
			}

			return nil
		},
	}
	cmd.Flags().StringVarP(&options.filename, "filename", "f", "", "CertificationPlan YAML file")
	cmd.Flags().StringVar(
		&options.bindings,
		"bindings",
		"",
		"CertificationPlanBindings YAML containing approved Test identities",
	)
	cmd.Flags().StringVar(
		&options.inputs,
		"inputs",
		"",
		"CertificationPlanInputs YAML containing environment-specific stage parameters",
	)
	cmd.Flags().BoolVar(
		&options.testsOnly,
		"tests-only",
		false,
		"render WorkflowTemplates and CertificationTests while bootstrapping a plan that defines a Suite",
	)
	cmd.MarkFlagsMutuallyExclusive("bindings", "tests-only")
	cmd.MarkFlagsMutuallyExclusive("inputs", "tests-only")

	if err := cmd.MarkFlagRequired("filename"); err != nil {
		panic(err)
	}

	return cmd
}
