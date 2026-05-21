// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
)

// newCacheCmd is the `cache` parent command. Subcommands operate on
// the orca cachestore directly, bypassing the daemon. Useful for
// answering "did the last roundtrip actually populate the cache?"
// and for forcing a cold-cache state before benchmarking.
func newCacheCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect or clear the orca chunk cachestore",
	}

	cmd.AddCommand(newCacheListCmd(g))
	cmd.AddCommand(newCacheInspectCmd(g))
	cmd.AddCommand(newCacheClearCmd(g))

	return cmd
}

// --- cache list ---

type cacheListOpts struct {
	prefix    string
	groupBy   string
	limit     int
	originAll bool
}

func newCacheListCmd(g *globalFlags) *cobra.Command {
	o := &cacheListOpts{limit: 1000}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "Enumerate chunks in the cachestore",
		Long: `List walks the cachestore S3 bucket for the configured origin and
prints either per-chunk rows (default) or one row per object
(--group-by hash). Chunk paths follow
"<origin_id>/<sha256-hex>/<index>"; the hash is one-way over
(origin_id, bucket, key, etag, chunk_size) so individual chunks
cannot be decoded back to a human-readable name without
side-information.

Use cache inspect when you have a (bucket, key) in hand and want
to know whether its chunks are present.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCacheList(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.prefix, "prefix", "", "limit results to chunk paths starting with this prefix")
	cmd.Flags().StringVar(&o.groupBy, "group-by", "", "set to 'hash' to aggregate one row per object")
	cmd.Flags().IntVar(&o.limit, "limit", o.limit, "max rows to print (0 = unlimited; default 1000)")
	cmd.Flags().BoolVar(&o.originAll, "all-origins", false, "list across all origin_ids (default: scope to --origin-id)")

	return cmd
}

func runCacheList(ctx context.Context, g *globalFlags, o *cacheListOpts) error {
	cs, err := newCachestoreClient(ctx, g)
	if err != nil {
		return err
	}

	prefix := o.prefix
	if prefix == "" && !o.originAll && g.originID != "" {
		prefix = g.originID + "/"
	}

	objs, err := cs.List(ctx, prefix, o.limit)
	if err != nil {
		return err
	}

	if o.groupBy == "hash" {
		groups := groupCacheByHash(objs)

		fmt.Printf("%-72s\t%-8s\t%-12s\n", "ORIGIN/HASH", "CHUNKS", "TOTAL")

		for _, gr := range groups {
			fmt.Printf("%-72s\t%-8d\t%-12s\n", gr.OriginHash, gr.Chunks, formatSize(gr.Total))
		}

		fmt.Fprintf(os.Stderr, "(%d objects, %d chunks)\n", len(groups), len(objs))

		return nil
	}

	fmt.Printf("%-80s\t%-12s\t%s\n", "PATH", "SIZE", "LAST_MODIFIED")

	var total int64
	for _, ob := range objs {
		fmt.Printf("%-80s\t%-12s\t%s\n", ob.Path, formatSize(ob.Size), ob.LastModified.UTC().Format("2006-01-02T15:04:05Z"))
		total += ob.Size
	}

	fmt.Fprintf(os.Stderr, "(%d chunks, %s total)\n", len(objs), formatSize(total))

	if o.limit > 0 && len(objs) >= o.limit {
		fmt.Fprintf(os.Stderr, "  ... output truncated at --limit %d; pass --limit 0 for unlimited\n", o.limit)
	}

	return nil
}

// cacheGroup is one row in the --group-by hash output: all chunks
// of a single object collapsed into a single line.
type cacheGroup struct {
	OriginHash string // "<origin_id>/<hash>"
	Chunks     int
	Total      int64
}

func groupCacheByHash(objs []cacheObject) []cacheGroup {
	idx := make(map[string]*cacheGroup)
	order := []string{}

	for _, ob := range objs {
		oh := originHashPrefix(ob.Path)
		if oh == "" {
			continue
		}

		gr, ok := idx[oh]
		if !ok {
			gr = &cacheGroup{OriginHash: oh}
			idx[oh] = gr

			order = append(order, oh)
		}

		gr.Chunks++
		gr.Total += ob.Size
	}

	out := make([]cacheGroup, 0, len(order))
	for _, oh := range order {
		out = append(out, *idx[oh])
	}

	return out
}

// originHashPrefix returns "<origin_id>/<hash>" given a full chunk
// path "<origin_id>/<hash>/<index>". Returns "" if the path doesn't
// have at least two segments before the index suffix.
func originHashPrefix(path string) string {
	// Find the last slash; everything before it is "<origin>/<hash>".
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return ""
	}

	return path[:idx]
}

// --- cache inspect ---

type cacheInspectOpts struct {
	bucket    string
	key       string
	etag      string
	chunkSize string
}

func newCacheInspectCmd(g *globalFlags) *cobra.Command {
	o := &cacheInspectOpts{}

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Show which chunks of an object are present in the cachestore",
		Long: `Inspect HEADs the origin for the named object to learn its size
and ETag, computes the canonical chunk paths under
"<origin_id>/<sha256-hex>/<index>", then HEADs each path in the
cachestore. Prints per-chunk presence + size.

If --etag is supplied, the origin HEAD is skipped (useful for
inspecting chunks belonging to a since-deleted object).

If --chunk-size is supplied, it overrides the size resolved from
--config (or the orca default 8 MiB if no config is loaded).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCacheInspect(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.bucket, "bucket", "", "origin bucket / container (required)")
	cmd.Flags().StringVar(&o.key, "key", "", "object name (required)")
	cmd.Flags().StringVar(&o.etag, "etag", "", "object etag (default: resolve via origin HEAD)")
	cmd.Flags().StringVar(&o.chunkSize, "chunk-size", "", "chunk size for chunk-path computation (overrides --config)")

	return cmd
}

func runCacheInspect(ctx context.Context, g *globalFlags, o *cacheInspectOpts) error {
	if o.bucket == "" || o.key == "" {
		return fmt.Errorf("--bucket and --key are required")
	}

	if g.originID == "" {
		return fmt.Errorf("--origin-id is required (either via --config or explicit flag)")
	}

	chunkSize, err := resolveChunkSize(g, o.chunkSize)
	if err != nil {
		return err
	}

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}
	// Override the resolved bucket for this one operation so the
	// origin Head targets what the operator asked for, not the
	// configured default.
	g.originBucket = o.bucket

	cs, err := newCachestoreClient(ctx, g)
	if err != nil {
		return err
	}

	etag := o.etag
	var size int64
	if etag == "" {
		info, err := oc.Head(ctx, o.key)
		if err != nil {
			return fmt.Errorf("origin head: %w", err)
		}

		etag = info.ETag
		size = info.Size
	}

	if etag == "" {
		return fmt.Errorf("could not determine ETag; pass --etag explicitly")
	}

	// Determine number of chunks. If we know the object size, walk
	// through ceil(size/chunkSize); otherwise just probe until we
	// hit a not-found at index 0 (degenerate case).
	if size <= 0 {
		fmt.Fprintln(os.Stderr, "warning: object size unknown; will probe index 0 only")

		size = chunkSize
	}

	nChunks := (size + chunkSize - 1) / chunkSize

	fmt.Printf("origin HEAD: bucket=%s key=%s size=%s etag=%s chunk_size=%s expected_chunks=%d\n\n",
		o.bucket, o.key, formatSize(size), etag, formatSize(chunkSize), nChunks)
	fmt.Printf("%-6s\t%-80s\t%-8s\t%s\n", "INDEX", "PATH", "PRESENT", "SIZE")

	var (
		present int64
		bytes   int64
	)

	for i := int64(0); i < nChunks; i++ {
		k := chunk.Key{
			OriginID:  g.originID,
			Bucket:    o.bucket,
			ObjectKey: o.key,
			ETag:      etag,
			ChunkSize: chunkSize,
			Index:     i,
		}
		path := k.Path()

		info, err := cs.Head(ctx, path)
		switch {
		case errors.Is(err, ErrCacheNotFound):
			fmt.Printf("%-6d\t%-80s\t%-8s\t%s\n", i, path, "NO", "-")
		case err != nil:
			fmt.Printf("%-6d\t%-80s\t%-8s\t%s\n", i, path, "ERR", err.Error())
		default:
			fmt.Printf("%-6d\t%-80s\t%-8s\t%s\n", i, path, "yes", formatSize(info.Size))
			present++
			bytes += info.Size
		}
	}

	pct := 0.0
	if nChunks > 0 {
		pct = 100.0 * float64(present) / float64(nChunks)
	}

	fmt.Fprintf(os.Stderr, "\ncached: %d/%d chunks (%.1f%%) %s\n", present, nChunks, pct, formatSize(bytes))

	return nil
}

// resolveChunkSize returns the chunk size to use for chunk-path
// computation. Priority: --chunk-size flag > --config Chunking.Size
// > orca default 8 MiB.
func resolveChunkSize(g *globalFlags, override string) (int64, error) {
	if override != "" {
		return parseSize(override)
	}

	if g.configPath != "" {
		cfg, err := config.Load(g.configPath)
		if err == nil && cfg.Chunking.Size.Int64() > 0 {
			return cfg.Chunking.Size.Int64(), nil
		}
	}

	return 8 * 1024 * 1024, nil
}

// --- cache clear ---

type cacheClearOpts struct {
	all      bool
	originID string
	objectK  string // "bucket/key" form
	etag     string
	yes      bool
}

func newCacheClearCmd(g *globalFlags) *cobra.Command {
	o := &cacheClearOpts{}

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Delete chunks from the cachestore",
		Long: `Clear removes chunks from the cachestore in one of three modes:

  --all                 Delete every object in the cachestore bucket.
                        Requires --yes.
  --origin-id ID        Delete every chunk under "<ID>/" prefix.
  --object BUCKET/KEY   Compute the chunk paths for the named object
                        (uses origin HEAD for size + etag) and delete
                        only those chunks. Use --etag to skip the
                        origin HEAD.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCacheClear(cmd.Context(), g, o)
		},
	}

	cmd.Flags().BoolVar(&o.all, "all", false, "delete every chunk in the cachestore bucket (requires --yes)")
	cmd.Flags().StringVar(&o.originID, "origin-id", "", "delete every chunk under this origin_id prefix")
	cmd.Flags().StringVar(&o.objectK, "object", "", "delete chunks for one object, format BUCKET/KEY")
	cmd.Flags().StringVar(&o.etag, "etag", "", "object etag (used with --object; default: resolve via origin HEAD)")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "skip the interactive confirmation prompt")

	return cmd
}

func runCacheClear(ctx context.Context, g *globalFlags, o *cacheClearOpts) error {
	cs, err := newCachestoreClient(ctx, g)
	if err != nil {
		return err
	}

	switch {
	case o.all:
		if !o.yes {
			return fmt.Errorf("--all requires --yes for safety")
		}

		return clearByPrefix(ctx, cs, "")

	case o.originID != "":
		prefix := o.originID
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}

		if !o.yes {
			if err := confirmPrompt(fmt.Sprintf("delete every chunk under %q?", prefix)); err != nil {
				return err
			}
		}

		return clearByPrefix(ctx, cs, prefix)

	case o.objectK != "":
		bucket, key := splitBucketKey(o.objectK)
		if bucket == "" || key == "" {
			return fmt.Errorf("--object %q: must be BUCKET/KEY", o.objectK)
		}

		return clearByObject(ctx, g, cs, bucket, key, o.etag, o.yes)
	}

	return fmt.Errorf("one of --all, --origin-id, or --object is required")
}

func clearByPrefix(ctx context.Context, cs *cachestoreClient, prefix string) error {
	objs, err := cs.List(ctx, prefix, 0)
	if err != nil {
		return err
	}

	if len(objs) == 0 {
		fmt.Fprintf(os.Stderr, "no chunks match prefix %q\n", prefix)
		return nil
	}

	for _, ob := range objs {
		if err := cs.Delete(ctx, ob.Path); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "deleted %d chunks under %q\n", len(objs), prefix)

	return nil
}

func clearByObject(ctx context.Context, g *globalFlags, cs *cachestoreClient, bucket, key, etag string, yes bool) error {
	if g.originID == "" {
		return fmt.Errorf("--origin-id required (set via --config or flag) to compute chunk paths")
	}

	chunkSize, err := resolveChunkSize(g, "")
	if err != nil {
		return err
	}

	// Need an origin client to HEAD for size + etag (if not given).
	g.originBucket = bucket

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	size := int64(0)
	if etag == "" {
		info, err := oc.Head(ctx, key)
		if err != nil {
			return fmt.Errorf("origin head: %w", err)
		}

		etag = info.ETag
		size = info.Size
	}

	if etag == "" {
		return fmt.Errorf("could not determine ETag; pass --etag explicitly")
	}
	// If size unknown, probe and delete indices until we exhaust
	// the chain. We cap at 1024 to avoid runaway loops.
	if size <= 0 {
		size = chunkSize * 1024
	}

	nChunks := (size + chunkSize - 1) / chunkSize

	if !yes {
		if err := confirmPrompt(fmt.Sprintf("delete up to %d chunks for %s/%s?", nChunks, bucket, key)); err != nil {
			return err
		}
	}

	var deleted int64

	for i := int64(0); i < nChunks; i++ {
		k := chunk.Key{
			OriginID:  g.originID,
			Bucket:    bucket,
			ObjectKey: key,
			ETag:      etag,
			ChunkSize: chunkSize,
			Index:     i,
		}
		path := k.Path()
		// Delete is idempotent so we just issue it; if the chunk
		// was already absent the call still succeeds. To get a
		// meaningful count, HEAD first.
		if _, err := cs.Head(ctx, path); err != nil {
			if errors.Is(err, ErrCacheNotFound) {
				continue
			}

			return err
		}

		if err := cs.Delete(ctx, path); err != nil {
			return err
		}

		deleted++
	}

	fmt.Fprintf(os.Stderr, "deleted %d chunks for %s/%s\n", deleted, bucket, key)

	return nil
}

// splitBucketKey parses "bucket/key" into (bucket, key). Returns
// ("","") if the input has no slash.
func splitBucketKey(s string) (string, string) {
	idx := strings.IndexByte(s, '/')
	if idx <= 0 || idx == len(s)-1 {
		return "", ""
	}

	return s[:idx], s[idx+1:]
}

// confirmPrompt asks for y/N confirmation on stdin. Returns an
// error wrapping the user response when the user declines or stdin
// is closed.
func confirmPrompt(msg string) error {
	fmt.Fprintf(os.Stderr, "%s proceed? [y/N]: ", msg)

	r := bufio.NewReader(os.Stdin)

	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("stdin closed without input; pass --yes to skip the prompt")
		}

		return fmt.Errorf("read confirmation: %w", err)
	}

	if strings.ToLower(strings.TrimSpace(line)) != "y" {
		return fmt.Errorf("aborted")
	}

	return nil
}
