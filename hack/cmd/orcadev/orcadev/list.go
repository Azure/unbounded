// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

type listOpts struct {
	prefix string
	limit  int
}

func newListCmd(g *globalFlags) *cobra.Command {
	o := &listOpts{}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List objects in the origin bucket / container",
		Long: `List prints "<size>\t<name>" for each object in the configured
origin, optionally filtered by --prefix. Works against both awss3
and azureblob drivers.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.prefix, "prefix", "", "only list objects whose name starts with this prefix")
	cmd.Flags().IntVar(&o.limit, "limit", 0, "maximum entries to print (0 = unlimited)")

	return cmd
}

func runList(ctx context.Context, g *globalFlags, o *listOpts) error {
	cleanup, err := ensurePortForwards(ctx, g)
	if err != nil {
		return err
	}

	defer cleanup()

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	objs, err := oc.List(ctx, o.prefix, o.limit)
	if err != nil {
		return err
	}

	var total int64
	for _, obj := range objs {
		fmt.Printf("%-12s\t%s\n", formatSize(obj.Size), obj.Name)

		total += obj.Size
	}

	fmt.Fprintf(os.Stderr, "(%d objects, %s total)\n", len(objs), formatSize(total))

	return nil
}

type deleteOpts struct {
	prefix string
	yes    bool
}

func newDeleteCmd(g *globalFlags) *cobra.Command {
	o := &deleteOpts{}

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete objects from the origin bucket / container",
		Long: `Delete removes every object in the origin whose name begins with
--prefix (default: all objects). Without --yes the command lists
the matching set and prompts for confirmation on stdin.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDelete(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.prefix, "prefix", "", "only delete objects whose name starts with this prefix (empty = all)")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "skip the interactive confirmation prompt")

	return cmd
}

func runDelete(ctx context.Context, g *globalFlags, o *deleteOpts) error {
	cleanup, err := ensurePortForwards(ctx, g)
	if err != nil {
		return err
	}

	defer cleanup()

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	objs, err := oc.List(ctx, o.prefix, 0)
	if err != nil {
		return err
	}

	if len(objs) == 0 {
		fmt.Fprintf(os.Stderr, "no matching objects in %s\n", oc.Bucket())
		return nil
	}

	if !o.yes {
		fmt.Fprintf(os.Stderr, "about to delete %d object(s) from %s:\n", len(objs), oc.Bucket())

		for _, obj := range objs {
			fmt.Fprintf(os.Stderr, "  %s\n", obj.Name)
		}

		if err := confirmPrompt(""); err != nil {
			if errors.Is(err, errConfirmAborted) {
				fmt.Fprintln(os.Stderr, "aborted.")
				return nil
			}

			return err
		}
	}

	for _, obj := range objs {
		if err := oc.Delete(ctx, obj.Name); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "deleted %d objects from %s\n", len(objs), oc.Bucket())

	return nil
}
