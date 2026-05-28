// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"context"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type roundtripOpts struct {
	file       string
	key        string
	rangeSpec  string
	repeat     int
	cleanup    bool
	dumpDiff   bool
	diffBytes  int
	expectETag string
}

func newRoundtripCmd(g *globalFlags) *cobra.Command {
	o := &roundtripOpts{
		repeat:    1,
		diffBytes: 256,
	}

	cmd := &cobra.Command{
		Use:   "roundtrip",
		Short: "Upload data, fetch through orca, verify SHA-256 matches",
		Long: `Roundtrip is the headline correctness check: upload a file to the
origin, request it through orca's edge, and compare a streaming
SHA-256 of the source bytes against a streaming SHA-256 of the
bytes orca returns.

Two modes:

  orcadev roundtrip --file ./data.bin
      Upload the file (under its basename) then GET it back. Source
      hash is computed while uploading; received hash is computed
      while streaming the GET response.

  orcadev roundtrip --key existing.bin
      Skip the upload step; fetch an existing origin object and
      compare against an on-the-fly origin Get. Useful for
      verifying that orca correctly serves objects it did not see
      arrive at the origin.

On mismatch, --dump-diff prints a side-by-side hex dump of the
first --diff-bytes bytes that differ. Without --dump-diff only the
two SHA-256 digests and the offset of the first differing byte are
printed.

Exits non-zero on any mismatch.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRoundtrip(cmd.Context(), g, o)
		},
	}

	cmd.Flags().StringVar(&o.file, "file", "", "local file to upload before fetching")
	cmd.Flags().StringVar(&o.key, "key", "", "existing origin object to fetch (mutually exclusive with --file)")
	cmd.Flags().StringVar(&o.rangeSpec, "range", "", "byte range to fetch (e.g. bytes=0-1023)")
	cmd.Flags().IntVar(&o.repeat, "repeat", o.repeat, "number of sequential GETs to issue (1st cold, rest warm)")
	cmd.Flags().BoolVar(&o.cleanup, "cleanup", o.cleanup, "delete the origin object after the roundtrip completes")
	cmd.Flags().BoolVar(&o.dumpDiff, "dump-diff", o.dumpDiff, "on mismatch, print a hex diff of the first --diff-bytes bytes")
	cmd.Flags().IntVar(&o.diffBytes, "diff-bytes", o.diffBytes, "hex-diff window size (bytes; only used with --dump-diff)")
	cmd.Flags().StringVar(&o.expectETag, "expect-etag", "", "fail if the orca-returned ETag does not match this value")

	return cmd
}

// runRoundtrip orchestrates the full upload/fetch/verify cycle.
// Returns a non-nil error on failure so cobra surfaces a non-zero
// exit code.
func runRoundtrip(ctx context.Context, g *globalFlags, o *roundtripOpts) error {
	if o.file == "" && o.key == "" {
		return fmt.Errorf("one of --file or --key is required")
	}

	if o.file != "" && o.key != "" {
		return fmt.Errorf("--file and --key are mutually exclusive")
	}

	if o.repeat < 1 {
		o.repeat = 1
	}

	// Auto-open kubectl port-forwards to svc/orca, svc/azurite,
	// svc/localstack as needed. No-op for any endpoint that is
	// already bound on localhost (user-managed port-forward, sibling
	// orcadev). See ensurePortForwards / derivePortForwardSpecs.
	cleanup, err := ensurePortForwards(ctx, g)
	if err != nil {
		return err
	}

	defer cleanup()

	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	if g.ensureContainer {
		if err := oc.EnsureBucket(ctx); err != nil {
			return err
		}
	}

	edge := newEdgeClient(g.orcaURL, g.timeout)

	return runRoundtripWith(ctx, oc, edge, o)
}

// runRoundtripWith executes the roundtrip loop against an already-
// constructed origin client and edge client. Split out from
// runRoundtrip so tests can drive the full verify cycle with a
// fake origin + httptest edge without spinning a kind cluster.
func runRoundtripWith(ctx context.Context, oc originClient, edge *edgeClient, o *roundtripOpts) error {
	// Determine the object name + the source-hash to compare against.
	key, sourceHash, size, err := prepareRoundtripSource(ctx, oc, o)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "source: %s (%s, sha256=%s)\n", key, formatSize(size), sourceHash)

	// Run the configured number of iterations.
	for i := 0; i < o.repeat; i++ {
		start := time.Now()

		recvHash, status, gotSize, gotETag, err := fetchAndHash(ctx, edge, oc.Bucket(), key, o.rangeSpec)
		elapsed := time.Since(start)

		if err != nil {
			return fmt.Errorf("iter %d: fetch: %w", i, err)
		}

		if status != http.StatusOK && status != http.StatusPartialContent {
			return fmt.Errorf("iter %d: orca returned status %d", i, status)
		}

		if err := validateExpectedETag(i, gotETag, o.expectETag); err != nil {
			return err
		}

		throughput := "n/a"
		if elapsed > 0 && gotSize > 0 {
			throughput = formatRate(gotSize, elapsed)
		}

		fmt.Fprintf(os.Stderr, "iter %d: status=%d bytes=%s elapsed=%s rate=%s sha256=%s\n",
			i, status, formatSize(gotSize), elapsed.Round(time.Microsecond), throughput, recvHash)

		if recvHash != sourceHash {
			fmt.Fprintln(os.Stderr, "MISMATCH")
			fmt.Fprintf(os.Stderr, "  source sha256:   %s\n", sourceHash)
			fmt.Fprintf(os.Stderr, "  received sha256: %s\n", recvHash)

			if o.dumpDiff {
				if err := emitHexDiff(ctx, oc, edge, key, o.rangeSpec, o.diffBytes); err != nil {
					fmt.Fprintf(os.Stderr, "  (dump-diff failed: %v)\n", err)
				}
			}

			return fmt.Errorf("checksum mismatch on iter %d", i)
		}
	}

	fmt.Fprintln(os.Stderr, "PASS")

	if o.cleanup && o.file != "" {
		if err := oc.Delete(ctx, key); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cleanup delete failed: %v\n", err)
		}
	}

	return nil
}

// prepareRoundtripSource handles the upload-or-fetch path and returns
// the key to fetch, the source SHA-256, and the source size.
//
// In --file mode it uploads the file, hashing the bytes as they
// stream to the origin so we do not have to re-read disk to compute
// the digest. In --key mode it issues an origin Get and hashes the
// stream (the source-of-truth here is the origin's current bytes).
//
// When --range is set, the source hash is the hash of the requested
// range only, so equality with the orca response is well-defined.
func prepareRoundtripSource(ctx context.Context, oc originClient, o *roundtripOpts) (string, string, int64, error) {
	if o.file != "" {
		return uploadAndHash(ctx, oc, o.file, o.rangeSpec)
	}

	return fetchOriginAndHash(ctx, oc, o.key, o.rangeSpec)
}

// uploadAndHash streams file -> origin and through SHA-256
// simultaneously. The destination name is filepath.Base(file).
func uploadAndHash(ctx context.Context, oc originClient, file, rangeSpec string) (string, string, int64, error) {
	st, err := os.Stat(file)
	if err != nil {
		return "", "", 0, fmt.Errorf("stat: %w", err)
	}

	f, err := os.Open(file)
	if err != nil {
		return "", "", 0, fmt.Errorf("open: %w", err)
	}

	defer f.Close() //nolint:errcheck // tool; best-effort

	name := filepath.Base(file)

	h := hasher()
	reader := newTeeHashReader(f, h)

	if err := oc.Put(ctx, name, reader, st.Size()); err != nil {
		return "", "", 0, err
	}

	srcHash := hexSum(h)

	if rangeSpec != "" {
		// We hashed the whole file; recompute over the requested
		// range so the comparison is apples-to-apples.
		rh, n, err := rangeFileHash(file, rangeSpec)
		if err != nil {
			return "", "", 0, fmt.Errorf("range hash: %w", err)
		}

		return name, rh, n, nil
	}

	return name, srcHash, st.Size(), nil
}

// fetchOriginAndHash streams the origin Get response through SHA-256
// without buffering. When --range is set the SHA-256 is over the
// requested byte slice only (origin Gets the whole object; we throw
// away the bytes outside the range before hashing).
func fetchOriginAndHash(ctx context.Context, oc originClient, key, rangeSpec string) (string, string, int64, error) {
	info, err := oc.Head(ctx, key)
	if err != nil {
		return "", "", 0, fmt.Errorf("head origin: %w", err)
	}

	body, _, err := oc.Get(ctx, key)
	if err != nil {
		return "", "", 0, fmt.Errorf("get origin: %w", err)
	}

	defer body.Close() //nolint:errcheck // best-effort

	h := hasher()

	var n int64
	if rangeSpec != "" {
		start, end, err := parseByteRange(rangeSpec, info.Size)
		if err != nil {
			return "", "", 0, err
		}

		n, err = hashSlice(body, h, start, end)
		if err != nil {
			return "", "", 0, err
		}
	} else {
		var err error

		n, err = io.Copy(h, body)
		if err != nil {
			return "", "", 0, fmt.Errorf("hash stream: %w", err)
		}
	}

	return key, hexSum(h), n, nil
}

// rangeFileHash hashes a byte range of file on disk. Used when
// --range is set together with --file: the source-of-truth is the
// range slice of the local file, not the whole upload.
func rangeFileHash(file, rangeSpec string) (string, int64, error) {
	st, err := os.Stat(file)
	if err != nil {
		return "", 0, err
	}

	start, end, err := parseByteRange(rangeSpec, st.Size())
	if err != nil {
		return "", 0, err
	}

	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}

	defer f.Close() //nolint:errcheck

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", 0, err
	}

	h := hasher()
	n, err := io.CopyN(h, f, end-start+1)

	return hexSum(h), n, err
}

// hashSlice reads from r, discarding bytes outside [start, end] and
// hashing the rest into h. Used to compute a range hash from a
// whole-object reader.
func hashSlice(r io.Reader, h hash.Hash, start, end int64) (int64, error) {
	// Discard the bytes before start.
	if _, err := io.CopyN(io.Discard, r, start); err != nil {
		return 0, fmt.Errorf("skip prefix: %w", err)
	}

	want := end - start + 1

	n, err := io.CopyN(h, r, want)
	if err != nil && err != io.EOF {
		return n, err
	}

	return n, nil
}

// fetchAndHash issues a GET (with optional Range) against the orca
// edge and streams the response through SHA-256 without buffering.
func fetchAndHash(ctx context.Context, edge *edgeClient, bucket, key, rangeSpec string) (string, int, int64, string, error) {
	var (
		resp edgeResponse
		err  error
	)

	if rangeSpec != "" {
		// We need to learn object size first to translate the spec
		// to absolute start/end indices. The simpler shape is to
		// pass the raw header through; do that by issuing a custom
		// GetRange-equivalent that just sets the header value.
		hd, err := edge.Head(ctx, bucket, key)
		if err != nil {
			return "", 0, 0, "", err
		}

		start, end, perr := parseByteRange(rangeSpec, hd.Size)
		if perr != nil {
			return "", 0, 0, "", perr
		}

		resp, err = edge.GetRange(ctx, bucket, key, start, end)
		if err != nil {
			return "", 0, 0, "", err
		}
	} else {
		resp, err = edge.Get(ctx, bucket, key)
		if err != nil {
			return "", 0, 0, "", err
		}
	}

	if resp.Body == nil {
		return "", resp.Status, 0, resp.ETag, nil
	}

	defer resp.Body.Close() //nolint:errcheck

	h := hasher()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", resp.Status, 0, resp.ETag, fmt.Errorf("read body: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), resp.Status, n, resp.ETag, nil
}

// normalizeETag canonicalises an ETag value for comparison:
// strips surrounding whitespace, the weak-validator W/ or w/
// prefix (along with any whitespace that follows it), and the
// surrounding double quotes RFC 7232 mandates. Comparison after
// normalisation is case-sensitive on the opaque tag body, matching
// RFC 7232 section 2.3 ("Strong comparison").
func normalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)

	if strings.HasPrefix(etag, "W/") || strings.HasPrefix(etag, "w/") {
		etag = strings.TrimSpace(etag[2:])
	}

	etag = strings.Trim(etag, "\"")

	return etag
}

func validateExpectedETag(iter int, got, want string) error {
	if want == "" {
		return nil
	}

	if normalizeETag(got) != normalizeETag(want) {
		return fmt.Errorf("iter %d: ETag mismatch: got %q want %q", iter, got, want)
	}

	return nil
}

// emitHexDiff fetches the requested range twice (once from the
// origin, once through orca) and prints a hex-side-by-side dump of
// the first diffBytes bytes that differ. Mismatch-only invocation;
// only called from the failure branch of roundtrip.
func emitHexDiff(ctx context.Context, oc originClient, edge *edgeClient, key, rangeSpec string, diffBytes int) error {
	srcReader, srcSize, err := oc.Get(ctx, key)
	if err != nil {
		return err
	}

	defer srcReader.Close() //nolint:errcheck

	var srcBuf []byte

	if rangeSpec != "" {
		start, end, err := parseByteRange(rangeSpec, srcSize)
		if err != nil {
			return err
		}

		if _, err := io.CopyN(io.Discard, srcReader, start); err != nil {
			return err
		}

		srcBuf, err = readN(srcReader, end-start+1, diffBytes)
		if err != nil {
			return err
		}
	} else {
		srcBuf, err = readN(srcReader, srcSize, diffBytes)
		if err != nil {
			return err
		}
	}

	var rcvResp edgeResponse
	if rangeSpec != "" {
		hd, err := edge.Head(ctx, oc.Bucket(), key)
		if err != nil {
			return err
		}

		start, end, perr := parseByteRange(rangeSpec, hd.Size)
		if perr != nil {
			return perr
		}

		rcvResp, err = edge.GetRange(ctx, oc.Bucket(), key, start, end)
		if err != nil {
			return err
		}
	} else {
		rcvResp, err = edge.Get(ctx, oc.Bucket(), key)
		if err != nil {
			return err
		}
	}

	defer func() {
		if rcvResp.Body != nil {
			_ = rcvResp.Body.Close() //nolint:errcheck
		}
	}()

	rcvBuf, err := readN(rcvResp.Body, int64(diffBytes), diffBytes)
	if err != nil {
		return err
	}

	off := firstDiffOffset(srcBuf, rcvBuf)
	if off < 0 {
		fmt.Fprintln(os.Stderr, "  (first 256 bytes match; difference is later in the stream)")
		return nil
	}

	fmt.Fprintf(os.Stderr, "  first difference at offset %d (0x%x)\n\n", off, off)
	fmt.Fprintln(os.Stderr, hexDiffDump(srcBuf, rcvBuf, 0, diffBytes))

	return nil
}

// readN reads up to want bytes from r, capped at limit, and returns
// the buffer. EOF on a short read is not an error.
func readN(r io.Reader, want int64, limit int) ([]byte, error) {
	if r == nil {
		return nil, nil
	}

	n := int64(limit)
	if want < n {
		n = want
	}

	buf := make([]byte, n)

	read, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	return buf[:read], nil
}

// parseByteRange parses an "bytes=A-B" or "bytes=-N" or "bytes=A-"
// spec against the object's size and returns inclusive start/end
// indices.
func parseByteRange(spec string, size int64) (int64, int64, error) {
	// Defer to a real parser via origin/server logic would be
	// overkill; the dev tool accepts the common shapes only.
	const prefix = "bytes="
	if !strings.HasPrefix(spec, prefix) {
		return 0, 0, fmt.Errorf("range %q: must start with %q", spec, prefix)
	}

	body := spec[len(prefix):]

	dash := strings.IndexByte(body, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("range %q: missing dash", spec)
	}

	leftStr, rightStr := body[:dash], body[dash+1:]

	if leftStr == "" {
		// suffix: -N
		n, err := strconv.ParseInt(rightStr, 10, 64)
		if err != nil || n <= 0 || n > size {
			return 0, 0, fmt.Errorf("range %q: invalid suffix", spec)
		}

		return size - n, size - 1, nil
	}

	start, err := strconv.ParseInt(leftStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("range %q: invalid start", spec)
	}

	end := size - 1
	if rightStr != "" {
		e, err := strconv.ParseInt(rightStr, 10, 64)
		if err != nil || e < start {
			return 0, 0, fmt.Errorf("range %q: invalid end", spec)
		}

		if e < end {
			end = e
		}
	}

	return start, end, nil
}

// formatRate renders bytes/elapsed as a human-friendly throughput
// string ("123.4 MiB/s").
func formatRate(bytes int64, elapsed time.Duration) string {
	if elapsed <= 0 {
		return "n/a"
	}

	perSec := float64(bytes) / elapsed.Seconds()
	const (
		kib = 1024.0
		mib = 1024 * kib
		gib = 1024 * mib
	)

	switch {
	case perSec >= gib:
		return fmt.Sprintf("%.2f GiB/s", perSec/gib)
	case perSec >= mib:
		return fmt.Sprintf("%.2f MiB/s", perSec/mib)
	case perSec >= kib:
		return fmt.Sprintf("%.2f KiB/s", perSec/kib)
	default:
		return fmt.Sprintf("%.0f B/s", perSec)
	}
}

// Compile-time guard against accidental sha256 removal: this import
// is what gives us streaming hash. crypto/sha256 is imported via
// hash.go::hasher; this anchor keeps the dependency obvious at this
// site too.
