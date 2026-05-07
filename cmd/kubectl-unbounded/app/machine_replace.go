// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func machineReplaceCommand() *cobra.Command {
	var (
		ttl   int32
		force bool
	)

	cmd := &cobra.Command{
		Use:   "replace NAME",
		Short: "Destructively replace a machine through MachineOperation",
		Long: `Replace creates a MachineOperation CR requesting a host VM replacement.
The machine-ops-controller processes the operation through the external
provider by deleting and recreating the host VM with fresh bootstrap data.
Host-local OS disk state is destroyed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := ctrl.SetupSignalHandler()

			c, err := newMachineClient()
			if err != nil {
				return err
			}

			return runReplace(ctx, c, args[0], ttl, force)
		},
	}

	cmd.Flags().Int32Var(&ttl, "ttl", defaultTTLSeconds,
		"Seconds after completion before the MachineOperation CR is automatically deleted (0 to disable)")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation for destructive VM replacement")

	return cmd
}

func runReplace(ctx context.Context, c client.WithWatch, name string, ttlSeconds int32, force bool) error {
	if !force {
		if err := confirmReplace(name, os.Stdin, os.Stderr); err != nil {
			return err
		}
	}

	opName := fmt.Sprintf("%s-replace-%d", name, time.Now().Unix())

	if err := createMachineOperation(ctx, c, name, opName, v1alpha3.OperationHostReplace, ttlSeconds); err != nil {
		return err
	}

	printStep(fmt.Sprintf("Replacing Machine %s...", name))
	printConfig("operation", opName)
	fmt.Println()

	return watchMachineOperation(ctx, c, opName)
}

func confirmReplace(name string, in *os.File, out io.Writer) error {
	return confirmReplaceWithTerminal(name, in, out, isTerminal(in))
}

func confirmReplaceWithTerminal(name string, in io.Reader, out io.Writer, terminal bool) error {
	if !terminal {
		return fmt.Errorf("replace deletes and recreates the host VM; rerun with --force to confirm")
	}

	if _, err := fmt.Fprintf(out, "This will delete and recreate host VM %q, destroying OS disk state. Type the machine name to continue: ", name); err != nil {
		return fmt.Errorf("write confirmation prompt: %w", err)
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w; rerun with --force to confirm", err)
	}

	if strings.TrimSpace(line) != name {
		return fmt.Errorf("confirmation did not match machine name %q", name)
	}

	return nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}
