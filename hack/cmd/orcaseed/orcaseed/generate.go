// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcaseed

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
)

// generateOpts captures the per-command flags for the generate
// subcommand. Defaults are conservative (1 MiB x 1 blob) so an
// accidental invocation with no flags is harmless.
type generateOpts struct {
	sizeStr     string
	count       int
	prefix      string
	seed        int64
	concurrency int
	force       bool
}

const (
	// perBlobMax is the per-blob ceiling. Larger blobs require
	// --force to acknowledge. Picked at 1 GiB to match the operator's
	// stated cap and keep accidental "1TiB" typos from filling the
	// emulator's emptyDir.
	perBlobMax int64 = 1024 * 1024 * 1024
	// totalWarn is the cumulative-bytes threshold above which the
	// command logs a warning before proceeding. Sized to match
	// perBlobMax for symmetry.
	totalWarn int64 = 1024 * 1024 * 1024
)

func newGenerateCmd(g *globalFlags) *cobra.Command {
	o := &generateOpts{
		sizeStr:     "1MiB",
		count:       1,
		prefix:      "synth-",
		concurrency: 4,
	}

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate N synthetic blobs of size S and upload them",
		Long: `Generate creates --count blobs of --size random bytes each, named
<prefix>0, <prefix>1, ... and uploads them to the configured
container. Use --seed to make the byte stream reproducible across
runs (useful when comparing cache behaviour between experiments).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGenerate(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.sizeStr, "size", o.sizeStr,
		"per-blob size (e.g. 1MiB, 100MB, 1GiB)")
	cmd.Flags().IntVar(&o.count, "count", o.count,
		"number of blobs to generate")
	cmd.Flags().StringVar(&o.prefix, "prefix", o.prefix,
		"blob name prefix; blobs are named <prefix><index>")
	cmd.Flags().Int64Var(&o.seed, "seed", o.seed,
		"PRNG seed for deterministic content; 0 = use crypto/rand")
	cmd.Flags().IntVar(&o.concurrency, "concurrency", o.concurrency,
		"number of parallel uploads")
	cmd.Flags().BoolVar(&o.force, "force", o.force,
		"allow per-blob size > 1 GiB")

	return cmd
}

func runGenerate(ctx context.Context, g *globalFlags, o *generateOpts) error {
	if o.count < 1 {
		return fmt.Errorf("--count must be >= 1")
	}

	if o.concurrency < 1 {
		o.concurrency = 1
	}

	size, err := parseSize(o.sizeStr)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}

	if size < 0 {
		return fmt.Errorf("--size must be non-negative")
	}

	if size > perBlobMax && !o.force {
		return fmt.Errorf("--size %s exceeds per-blob ceiling %s; pass --force to override",
			formatSize(size), formatSize(perBlobMax))
	}

	total := size * int64(o.count)
	if total > totalWarn {
		fmt.Fprintf(os.Stderr, "warning: cumulative upload is %s (size %s x count %d); proceeding\n",
			formatSize(total), formatSize(size), o.count)
	}

	_, cc, err := g.newClients(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "generating %d blobs of %s (total %s) into container %q at %s\n",
		o.count, formatSize(size), formatSize(total), g.containerName, g.endpoint)

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

	g2, gctx := errgroup.WithContext(ctx)
	g2.SetLimit(o.concurrency)

	var seedMu sync.Mutex // serialises math/rand stream when --seed is set

	var seededSrc *mathrand.Rand
	if o.seed != 0 {
		// Single shared source so deterministic-seed runs produce
		// the same per-blob bytes in the same order regardless of
		// concurrency. Concurrent uploaders serialise through
		// seedMu when reading from it; the read is fast (a few
		// MiB/s into the body buffer) so the contention is minor.
		seededSrc = mathrand.New(mathrand.NewSource(o.seed)) //nolint:gosec // dev tool, deterministic-by-design
	}

	for i := 0; i < o.count; i++ {
		i := i

		g2.Go(func() error {
			name := fmt.Sprintf("%s%d", o.prefix, i)

			body := newRandomReader(size, seededSrc, &seedMu)

			bc := cc.NewBlockBlobClient(name)
			if _, err := bc.UploadStream(gctx, body, &blockblob.UploadStreamOptions{}); err != nil {
				return fmt.Errorf("upload %s: %w", name, err)
			}

			uploaded.Add(1)
			bytes.Add(size)

			return nil
		})
	}

	if err := g2.Wait(); err != nil {
		return err
	}

	<-progressDone

	fmt.Fprintf(os.Stderr, "done: %d blobs, %s total\n", o.count, formatSize(bytes.Load()))

	return nil
}

// newRandomReader returns an io.Reader producing exactly n bytes. If
// seeded != nil the bytes come from the shared math/rand source
// (deterministic per --seed); otherwise from crypto/rand. The source
// is shared so concurrent uploaders preserve order under --seed; the
// mutex serialises Reads through the seeded source.
func newRandomReader(n int64, seeded *mathrand.Rand, mu *sync.Mutex) io.Reader {
	if seeded == nil {
		return io.LimitReader(rand.Reader, n)
	}

	return &lockedSeededReader{src: seeded, remaining: n, mu: mu}
}

type lockedSeededReader struct {
	src       *mathrand.Rand
	remaining int64
	mu        *sync.Mutex
}

func (r *lockedSeededReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}

	want := int64(len(p))
	if want > r.remaining {
		want = r.remaining
	}

	r.mu.Lock()
	n, _ := r.src.Read(p[:want]) //nolint:errcheck // math/rand never errors
	r.mu.Unlock()

	r.remaining -= int64(n)
	if r.remaining == 0 {
		return n, io.EOF
	}

	return n, nil
}
