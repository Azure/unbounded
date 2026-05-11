// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

type deleteOpts struct {
	prefix string
	yes    bool
}

func newDeleteCmd(g *globalFlags) *cobra.Command {
	o := &deleteOpts{}

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete blobs from the container",
		Long: `Delete removes every blob in the container whose name begins with
--prefix (default: all blobs). Without --yes the command lists the
matching set and prompts for confirmation on stdin.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDelete(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.prefix, "prefix", "",
		"only delete blobs whose name begins with this prefix (empty = all)")
	cmd.Flags().BoolVar(&o.yes, "yes", false,
		"skip the interactive confirmation prompt")

	return cmd
}

func runDelete(ctx context.Context, g *globalFlags, o *deleteOpts) error {
	_, cc, err := g.newClients(ctx)
	if err != nil {
		return err
	}

	opts := &container.ListBlobsFlatOptions{}
	if o.prefix != "" {
		opts.Prefix = &o.prefix
	}

	var names []string

	pager := cc.NewListBlobsFlatPager(opts)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}

		for _, item := range page.Segment.BlobItems {
			if item.Name != nil {
				names = append(names, *item.Name)
			}
		}
	}

	if len(names) == 0 {
		fmt.Fprintf(os.Stderr, "no matching blobs in container %q\n", g.containerName)
		return nil
	}

	if !o.yes {
		fmt.Fprintf(os.Stderr, "about to delete %d blob(s) from container %q:\n",
			len(names), g.containerName)

		for _, n := range names {
			fmt.Fprintf(os.Stderr, "  %s\n", n)
		}

		fmt.Fprint(os.Stderr, "proceed? [y/N]: ")

		r := bufio.NewReader(os.Stdin)

		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}

		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(os.Stderr, "aborted.")
			return nil
		}
	}

	for _, n := range names {
		bc := cc.NewBlobClient(n)
		if _, err := bc.Delete(ctx, nil); err != nil {
			return fmt.Errorf("delete %s: %w", n, err)
		}
	}

	fmt.Fprintf(os.Stderr, "deleted %d blobs from container %q\n", len(names), g.containerName)

	return nil
}
