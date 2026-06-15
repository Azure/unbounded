// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/renameio/v2"
	"github.com/spf13/cobra"
)

// seedOptions holds the parsed flags for the seed subcommand.
type seedOptions struct {
	outDir       string
	objectSize   string
	keyPrefix    string
	count        int64
	totalSize    string
	concurrency  int
	seed         int64
	overwrite    bool
	skipExisting bool
}

// newSeedCommand builds the "seed" subcommand.
func newSeedCommand() *cobra.Command {
	opts := &seedOptions{
		objectSize:  "1.25GB",
		keyPrefix:   "soaks3/",
		totalSize:   "10GB",
		concurrency: runtime.NumCPU(),
		seed:        1,
	}

	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Generate deterministic test objects on the local filesystem",
		Long: "seed writes a deterministic object tree under --out-dir mirroring the\n" +
			"bucket key layout. Upload the tree to an origin bucket out of band; the\n" +
			"run subcommand reads the same keys back through an unbounded-storage frontend.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --count and --total-size are mutually exclusive. --total-size
			// is defaulted, so an explicit --count without an explicit
			// --total-size should win rather than collide with the default.
			if cmd.Flags().Changed("count") && !cmd.Flags().Changed("total-size") {
				opts.totalSize = ""
			}

			return runSeed(cmd.Context(), opts)
		},
	}

	fs := cmd.Flags()
	fs.StringVar(&opts.outDir, "out-dir", "", "Output directory for generated objects (required)")
	fs.StringVar(&opts.objectSize, "object-size", opts.objectSize, "Size of each object (e.g. 4MiB)")
	fs.StringVar(&opts.keyPrefix, "key-prefix", opts.keyPrefix, "Key prefix for generated objects")
	fs.Int64Var(&opts.count, "count", 0, "Number of objects to generate (mutually exclusive with --total-size)")
	fs.StringVar(&opts.totalSize, "total-size", opts.totalSize, "Total data set size, e.g. 10GiB (mutually exclusive with --count)")
	fs.IntVar(&opts.concurrency, "concurrency", opts.concurrency, "Number of concurrent writers")
	fs.Int64Var(&opts.seed, "seed", opts.seed, "Content seed for deterministic object data")
	fs.BoolVar(&opts.overwrite, "overwrite", false, "Overwrite existing object files")
	fs.BoolVar(&opts.skipExisting, "skip-existing", false, "Skip objects whose files already exist")

	return cmd
}

// runSeed executes the seed subcommand.
func runSeed(ctx context.Context, opts *seedOptions) error {
	if opts.outDir == "" {
		return errors.New("--out-dir is required")
	}

	if opts.overwrite && opts.skipExisting {
		return errors.New("--overwrite and --skip-existing are mutually exclusive")
	}

	if opts.concurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1, got %d", opts.concurrency)
	}

	objectSize, err := parseSize(opts.objectSize)
	if err != nil {
		return fmt.Errorf("--object-size: %w", err)
	}

	if objectSize <= 0 {
		return fmt.Errorf("--object-size must be positive, got %d", objectSize)
	}

	var totalSize int64
	if opts.totalSize != "" {
		totalSize, err = parseSize(opts.totalSize)
		if err != nil {
			return fmt.Errorf("--total-size: %w", err)
		}
	}

	count, err := deriveCount(opts.count, totalSize, objectSize)
	if err != nil {
		return err
	}

	km, err := newKeyModel(opts.keyPrefix, count)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return fmt.Errorf("create out-dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("[soaks3] seeding %d objects of %s (%s total) into %s\n",
		count, humanize.IBytes(uint64(objectSize)),
		humanize.IBytes(uint64(count*objectSize)), opts.outDir)

	start := time.Now()

	written, err := seedObjects(ctx, opts, km, objectSize)
	if err != nil {
		return err
	}

	// If seeding was interrupted, the on-disk tree only holds a subset of
	// the objects. Writing a manifest that claims the full count would make
	// a later `run --manifest` request never-seeded keys (all 404s for the
	// missing tail), so bail out before writing it.
	if err := ctx.Err(); err != nil {
		fmt.Printf("[soaks3] interrupted after %d/%d objects; manifest not written\n",
			written.Load(), count)

		return err
	}

	manifestPath := filepath.Join(opts.outDir, manifestName)
	if err := writeManifest(manifestPath, manifest{
		Count:      count,
		ObjectSize: objectSize,
		KeyPrefix:  km.prefix,
		Seed:       opts.seed,
	}); err != nil {
		return err
	}

	fmt.Printf("[soaks3] seeded %d objects in %s (manifest: %s)\n",
		written.Load(), time.Since(start).Round(time.Millisecond), manifestPath)

	return nil
}

// seedObjects writes all objects concurrently and returns the count written.
func seedObjects(ctx context.Context, opts *seedOptions, km keyModel, objectSize int64) (*atomic.Int64, error) {
	var (
		written  atomic.Int64
		skipped  atomic.Int64
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)

	indices := make(chan int64, opts.concurrency)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err

			cancel()
		})
	}

	for w := 0; w < opts.concurrency; w++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for idx := range indices {
				if ctx.Err() != nil {
					return
				}

				done, err := writeObject(opts, km, idx, objectSize)
				if err != nil {
					fail(err)
					return
				}

				if done {
					written.Add(1)
				} else {
					skipped.Add(1)
				}
			}
		}()
	}

	progressDone := startProgress(ctx, &written, &skipped, km.count)

feed:
	for i := int64(0); i < km.count; i++ {
		select {
		case <-ctx.Done():
			break feed
		case indices <- i:
		}
	}

	close(indices)
	wg.Wait()
	close(progressDone)

	if firstErr != nil {
		return &written, firstErr
	}

	return &written, nil
}

// writeObject writes (or skips) a single object. It returns true when the
// object was written.
func writeObject(opts *seedOptions, km keyModel, idx, objectSize int64) (bool, error) {
	relPath := km.relPathForIndex(idx)
	fullPath := filepath.Join(opts.outDir, relPath)

	if _, err := os.Stat(fullPath); err == nil {
		switch {
		case opts.skipExisting:
			return false, nil
		case !opts.overwrite:
			return false, fmt.Errorf("object %s already exists (use --overwrite or --skip-existing)", fullPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat %s: %w", fullPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return false, fmt.Errorf("create dir for %s: %w", fullPath, err)
	}

	r := newContentReader(opts.seed, idx, objectSize)

	pf, err := renameio.NewPendingFile(fullPath, renameio.WithPermissions(0o644))
	if err != nil {
		return false, fmt.Errorf("open pending file %s: %w", fullPath, err)
	}

	defer pf.Cleanup() //nolint:errcheck // Cleanup is a no-op after CloseAtomicallyReplace.

	if _, err := io.Copy(pf, r); err != nil {
		return false, fmt.Errorf("write %s: %w", fullPath, err)
	}

	if err := pf.CloseAtomicallyReplace(); err != nil {
		return false, fmt.Errorf("commit %s: %w", fullPath, err)
	}

	return true, nil
}

// startProgress launches a goroutine that logs seeding progress periodically.
// The returned channel must be closed to stop it.
func startProgress(ctx context.Context, written, skipped *atomic.Int64, total int64) chan struct{} {
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				w := written.Load()
				s := skipped.Load()
				fmt.Printf("[soaks3] progress: %d/%d written, %d skipped\n", w, total, s)
			}
		}
	}()

	return done
}

// parseSize parses a humanized byte size such as "4MiB" or "10GB".
func parseSize(s string) (int64, error) {
	n, err := humanize.ParseBytes(s)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	return int64(n), nil
}
