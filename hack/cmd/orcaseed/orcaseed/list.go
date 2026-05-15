// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

type listOpts struct {
	prefix string
}

func newListCmd(g *globalFlags) *cobra.Command {
	o := &listOpts{}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List blobs currently in the container",
		Long: `List prints "<size>\t<name>" for each blob in the configured
container, optionally filtered by --prefix.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.prefix, "prefix", "",
		"only list blobs whose name begins with this prefix")

	return cmd
}

func runList(ctx context.Context, g *globalFlags, o *listOpts) error {
	_, cc, err := g.newClients(ctx)
	if err != nil {
		return err
	}

	opts := &container.ListBlobsFlatOptions{}
	if o.prefix != "" {
		opts.Prefix = &o.prefix
	}

	pager := cc.NewListBlobsFlatPager(opts)

	var (
		count int
		total int64
	)

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}

		for _, item := range page.Segment.BlobItems {
			name := ""
			if item.Name != nil {
				name = *item.Name
			}

			size := int64(0)
			if item.Properties != nil && item.Properties.ContentLength != nil {
				size = *item.Properties.ContentLength
			}

			fmt.Printf("%-12s\t%s\n", formatSize(size), name)

			count++
			total += size
		}
	}

	fmt.Fprintf(os.Stderr, "(%d blobs, %s total)\n", count, formatSize(total))

	return nil
}
