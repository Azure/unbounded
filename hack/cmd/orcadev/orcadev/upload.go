// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// uploadOpts is the per-command flag set. Modes are mutually
// exclusive: --file uploads a single named file; --generate
// synthesises --count blobs of --size random bytes each.
type uploadOpts struct {
	// File mode.
	file string
	name string

	// Generate mode.
	generate    bool
	count       int
	sizeStr     string
	seed        int64
	concurrency int
	force       bool

	// Shared.
	printChecksum bool
}

const (
	// uploadPerBlobMax bounds the size of a single synthetic blob.
	// Larger sizes require --force to acknowledge.
	uploadPerBlobMax int64 = 1024 * 1024 * 1024
	// uploadTotalWarn is the cumulative-bytes threshold above which
	// the command logs a warning before proceeding.
	uploadTotalWarn int64 = 1024 * 1024 * 1024
	// uploadDefaultName is the default --name in --generate mode
	// when the operator does not pass one explicitly. Yields blobs
	// "synth1", "synth2", ... .
	uploadDefaultName = "synth"
)

func newUploadCmd(g *globalFlags) *cobra.Command {
	o := &uploadOpts{
		sizeStr:     "1MiB",
		count:       1,
		concurrency: 4,
	}

	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload data to the origin (file from disk, or N synthetic blobs)",
		Long: `Upload pushes test data into the configured origin bucket /
container. Two modes:

  orcadev upload --file ./data.tar.gz [--name foo.tar.gz]
      Stream a single file from disk. Default destination name is
      filepath.Base(--file).

  orcadev upload --generate --count 5 --size 10MiB [--name foo]
      Synthesise --count blobs of --size random bytes each, named
      <name>1, <name>2, ... <name>N. Default --name is "synth"
      (so the default output is "synth1", "synth2", ...). Set
      --seed for reproducible content across runs.

In both modes --print-checksum streams an extra SHA-256 over the
uploaded bytes and prints the digest, useful for later verification.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpload(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.file, "file", "", "local file to upload (mutually exclusive with --generate)")
	cmd.Flags().StringVar(&o.name, "name", "",
		"destination object name; in --generate mode the per-blob index "+
			"is appended (defaults to basename of --file in file mode, "+
			"or \"synth\" in --generate mode)")
	cmd.Flags().BoolVar(&o.generate, "generate", false, "synthesise --count blobs instead of uploading a file")
	cmd.Flags().StringVar(&o.sizeStr, "size", o.sizeStr, "per-blob size (e.g. 1MiB, 100MB, 1GiB)")
	cmd.Flags().IntVar(&o.count, "count", o.count, "number of blobs to generate")
	cmd.Flags().Int64Var(&o.seed, "seed", o.seed, "PRNG seed for deterministic content; 0 = crypto/rand")
	cmd.Flags().IntVar(&o.concurrency, "concurrency", o.concurrency, "parallel uploads in --generate mode")
	cmd.Flags().BoolVar(&o.force, "force", o.force, "allow per-blob size > 1 GiB")
	cmd.Flags().BoolVar(&o.printChecksum, "print-checksum", o.printChecksum, "print SHA-256 of each uploaded blob")

	return cmd
}

func runUpload(ctx context.Context, g *globalFlags, o *uploadOpts) error {
	if o.file == "" && !o.generate {
		return fmt.Errorf("one of --file or --generate is required")
	}

	if o.file != "" && o.generate {
		return fmt.Errorf("--file and --generate are mutually exclusive")
	}

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	if g.ensureContainer {
		if err := oc.EnsureBucket(ctx); err != nil {
			return err
		}
	}

	if o.file != "" {
		return runUploadFile(ctx, oc, o)
	}

	return runUploadGenerate(ctx, oc, o)
}

func runUploadFile(ctx context.Context, oc originClient, o *uploadOpts) error {
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

	f, err := os.Open(o.file)
	if err != nil {
		return fmt.Errorf("open --file: %w", err)
	}

	defer f.Close() //nolint:errcheck // upload tool; file close best-effort on success path

	fmt.Fprintf(os.Stderr, "uploading %s (%s) -> %s/%s [%s]\n",
		o.file, formatSize(st.Size()), oc.Bucket(), name, oc.Driver())

	var reader io.Reader = f

	h := hasher()
	if o.printChecksum {
		reader = newTeeHashReader(reader, h)
	}

	if err := oc.Put(ctx, name, reader, st.Size()); err != nil {
		return err
	}

	if o.printChecksum {
		printOut("%s\t%d\t%s\n", hexSum(h), st.Size(), name)
	} else {
		fmt.Fprintln(os.Stderr, "done.")
	}

	return nil
}

func runUploadGenerate(ctx context.Context, oc originClient, o *uploadOpts) error {
	if o.count < 1 {
		return fmt.Errorf("--count must be >= 1")
	}

	if o.concurrency < 1 {
		o.concurrency = 1
	}

	// In --generate mode --name doubles as the per-blob base name;
	// the index 1..count is appended to produce the destination
	// object name. An empty --name falls back to "synth" so the
	// command works without surprises if the operator forgets the
	// flag.
	name := o.name
	if name == "" {
		name = uploadDefaultName
	}

	size, err := parseSize(o.sizeStr)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}

	if size < 0 {
		return fmt.Errorf("--size must be non-negative")
	}

	if size > uploadPerBlobMax && !o.force {
		return fmt.Errorf("--size %s exceeds per-blob ceiling %s; pass --force to override",
			formatSize(size), formatSize(uploadPerBlobMax))
	}

	total := size * int64(o.count)
	if total > uploadTotalWarn {
		fmt.Fprintf(os.Stderr, "warning: cumulative upload is %s (size %s x count %d); proceeding\n",
			formatSize(total), formatSize(size), o.count)
	}

	fmt.Fprintf(os.Stderr, "generating %d blobs of %s (total %s) -> %s [%s]\n",
		o.count, formatSize(size), formatSize(total), oc.Bucket(), oc.Driver())

	var (
		uploaded atomic.Int64
		bytes    atomic.Int64
	)

	progressDone := make(chan struct{})

	go func() {
		defer close(progressDone)

		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				done := uploaded.Load()
				if done >= int64(o.count) {
					return
				}

				fmt.Fprintf(os.Stderr, "  ... uploaded %d / %d (%s)\n",
					done, o.count, formatSize(bytes.Load()))
			}
		}
	}()

	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(o.concurrency)

	type checksumEntry struct {
		Name string
		Hex  string
		Size int64
	}

	checksums := make([]checksumEntry, o.count)

	// 1-based loop: blobs are named <name>1, <name>2, ..., <name>N.
	// 1-based indexing was chosen over 0-based so the single-blob
	// case (--count 1) yields "<name>1" rather than "<name>0",
	// which reads more naturally for an operator typing
	// `orcadev bench KEY=foo1`.
	for i := 1; i <= o.count; i++ {
		i := i

		eg.Go(func() error {
			blobName := fmt.Sprintf("%s%d", name, i)
			body := newRandomReader(size, o.seed, int64(i))

			h := hasher()

			reader := io.Reader(body)
			if o.printChecksum {
				reader = newTeeHashReader(reader, h)
			}

			if err := oc.Put(gctx, blobName, reader, size); err != nil {
				return err
			}

			uploaded.Add(1)
			bytes.Add(size)

			if o.printChecksum {
				// Slice is 0-indexed; i is 1-based.
				checksums[i-1] = checksumEntry{Name: blobName, Hex: hexSum(h), Size: size}
			}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	<-progressDone

	if o.printChecksum {
		for _, e := range checksums {
			printOut("%s\t%d\t%s\n", e.Hex, e.Size, e.Name)
		}
	}

	fmt.Fprintf(os.Stderr, "done: %d blobs, %s total\n", o.count, formatSize(bytes.Load()))

	return nil
}

// newRandomReader returns an io.Reader producing exactly n random
// bytes. When userSeed == 0 the bytes come from crypto/rand; when
// userSeed != 0 each blob's stream is derived from
// math/rand.NewSource(userSeed + blobIndex) so determinism survives
// --concurrency > 1.
func newRandomReader(n, userSeed, blobIndex int64) io.Reader {
	if userSeed == 0 {
		return io.LimitReader(rand.Reader, n)
	}

	src := mathrand.NewSource(userSeed + blobIndex)

	return &seededReader{rng: mathrand.New(src), remaining: n} //nolint:gosec // dev tool, deterministic-by-design
}

type seededReader struct {
	rng       *mathrand.Rand
	remaining int64
}

func (r *seededReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}

	want := int64(len(p))
	if want > r.remaining {
		want = r.remaining
	}

	n, _ := r.rng.Read(p[:want]) //nolint:errcheck // math/rand never errors

	r.remaining -= int64(n)
	if r.remaining == 0 {
		return n, io.EOF
	}

	return n, nil
}
