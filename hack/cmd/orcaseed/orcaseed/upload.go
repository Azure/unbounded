// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

type uploadOpts struct {
	file string
	name string
}

func newUploadCmd(g *globalFlags) *cobra.Command {
	o := &uploadOpts{}

	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload a single file from disk into the container",
		Long: `Upload reads --file from local disk and stores it in the configured
container under --name (default: filepath.Base(--file)). The
upload streams in chunks; very large files don't buffer in memory.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpload(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.file, "file", "", "local file to upload (required)")
	cmd.Flags().StringVar(&o.name, "name", "",
		"destination blob name (default: basename of --file)")

	return cmd
}

func runUpload(ctx context.Context, g *globalFlags, o *uploadOpts) error {
	if o.file == "" {
		return fmt.Errorf("--file is required")
	}

	st, err := os.Stat(o.file)
	if err != nil {
		return fmt.Errorf("stat --file: %w", err)
	}

	if st.IsDir() {
		return fmt.Errorf("--file %q is a directory; only single files are supported", o.file)
	}

	name := o.name
	if name == "" {
		name = filepath.Base(o.file)
	}

	_, cc, err := g.newClients(ctx)
	if err != nil {
		return err
	}

	f, err := os.Open(o.file)
	if err != nil {
		return fmt.Errorf("open --file: %w", err)
	}

	defer f.Close() //nolint:errcheck // upload tool, file close best-effort on success path

	fmt.Fprintf(os.Stderr, "uploading %s (%s) -> %s/%s\n",
		o.file, formatSize(st.Size()), g.containerName, name)

	bc := cc.NewBlockBlobClient(name)
	if _, err := bc.UploadStream(ctx, f, &blockblob.UploadStreamOptions{}); err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	fmt.Fprintf(os.Stderr, "done.\n")

	return nil
}
