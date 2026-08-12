// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/version"
)

const (
	defaultNamespace = "gantry-system"
	defaultTimeout   = 10 * time.Minute
)

type rootOptions struct {
	kubeconfig string
	context    string
	namespace  string
	timeout    time.Duration
	clients    *clusterClients
}

// NewCommand constructs the independent Gantry management CLI.
func NewCommand() *cobra.Command {
	options := &rootOptions{}
	command := &cobra.Command{
		Use:           "gantryctl",
		Short:         "Install and configure standalone Gantry",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.PersistentFlags().StringVar(&options.kubeconfig, "kubeconfig", "", "Path to kubeconfig (defaults to standard kubectl loading rules)")
	command.PersistentFlags().StringVar(&options.context, "context", "", "Kubeconfig context override")
	command.PersistentFlags().StringVar(&options.namespace, "namespace", defaultNamespace, "Namespace for the standalone Gantry installation")
	command.PersistentFlags().DurationVar(&options.timeout, "timeout", defaultTimeout, "Timeout for cluster operations")

	command.AddCommand(newInstallCommand(options))
	command.AddCommand(newRegistryCommand(options))
	command.AddCommand(newUninstallCommand(options))
	command.AddCommand(version.Command())

	return command
}

// Run executes gantryctl and exits nonzero on failure.
func Run() {
	command := NewCommand()
	if err := command.Execute(); err != nil {
		_, _ = fmt.Fprintln(command.ErrOrStderr(), err) //nolint:errcheck // terminal error reporting is best effort

		os.Exit(1)
	}
}

func writeOutputf(output io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}

	return nil
}

func defaultGantryImage() string {
	tag := strings.TrimSpace(version.Version)
	if tag == "" {
		tag = "dev"
	}

	if tag != "dev" && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}

	return "ghcr.io/azure/gantry:" + tag
}
