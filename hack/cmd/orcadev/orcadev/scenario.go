// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/orca/chunk"
)

// scenarioOpts captures the flag set shared by every scenario plus
// per-scenario knobs that are common enough to expose globally.
type scenarioOpts struct {
	sizeStr   string
	output    string
	jsonOut   string
	label     string
	keepData  bool
	chunkSize string
}

// scenarioResult is the JSON envelope emitted by --output json /
// --json-out for scenario runs. Mirrors benchResult but the
// payload is per-step rather than aggregate latency.
type scenarioResult struct {
	SchemaVersion int            `json:"schema_version"`
	Tool          string         `json:"tool"`
	Subcommand    string         `json:"subcommand"`
	Scenario      string         `json:"scenario"`
	Label         string         `json:"label,omitempty"`
	StartedAt     string         `json:"started_at"`
	FinishedAt    string         `json:"finished_at"`
	Config        map[string]any `json:"config"`
	Result        string         `json:"result"` // "pass" | "fail"
	FailureReason string         `json:"failure_reason,omitempty"`
	Steps         []scenarioStep `json:"steps"`
}

type scenarioStep struct {
	Name           string         `json:"name"`
	OK             bool           `json:"ok"`
	ElapsedSeconds float64        `json:"elapsed_seconds,omitempty"`
	Details        map[string]any `json:"details,omitempty"`
	Error          string         `json:"error,omitempty"`
}

func newScenarioCmd(g *globalFlags) *cobra.Command {
	o := &scenarioOpts{
		sizeStr: "4MiB",
		output:  "text",
	}

	cmd := &cobra.Command{
		Use:   "scenario [name]",
		Short: "Run a canned end-to-end scenario",
		Long: `Scenarios string together multiple orcadev primitives (upload,
roundtrip, cache inspect, cache clear) to exercise specific orca
behaviors end-to-end. Each scenario emits PASS / FAIL plus per-step
timings.

Available scenarios:

  cold-warm       Upload a blob, drop its cache, time cold + warm
                  GETs, report cache speedup ratio.
  range-stress    Many concurrent ranges across one large blob;
                  verify all bytes match the source.
  empty-object    Upload a zero-byte blob; verify GET returns 200
                  with empty body.
  etag-change     Upload v1, GET via orca (caches), overwrite at
                  origin (new content under same key, new etag),
                  GET again, verify orca serves the new bytes.

Use --label to tag a run; the value is baked into JSON for
cross-run comparison.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenario(cmd.Context(), g, o, args[0])
		},
	}

	cmd.Flags().StringVar(&o.sizeStr, "size", o.sizeStr, "blob size for scenarios that synthesize data")
	cmd.Flags().StringVar(&o.output, "output", o.output, "stdout format: text or json")
	cmd.Flags().StringVar(&o.jsonOut, "json-out", "", "write JSON result to this path")
	cmd.Flags().StringVar(&o.label, "label", "", "user-supplied tag baked into JSON for cross-run comparison")
	cmd.Flags().BoolVar(&o.keepData, "keep-data", false, "skip cleanup of origin objects and cache chunks at the end")
	cmd.Flags().StringVar(&o.chunkSize, "chunk-size", "", "chunk size for chunk-path computation (overrides --config)")

	return cmd
}

func runScenario(ctx context.Context, g *globalFlags, o *scenarioOpts, name string) error {
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("--output must be 'text' or 'json'")
	}

	// Auto-start a kubectl port-forward to svc/orca if needed.
	// Lifted to the parent so every scenario's edgeClient sees
	// the same forwarded socket without re-probing.
	cleanup, err := ensureEdgeReachable(ctx, g)
	if err != nil {
		return err
	}

	defer cleanup()

	startedAt := time.Now()

	res := &scenarioResult{
		SchemaVersion: 1,
		Tool:          "orcadev",
		Subcommand:    "scenario",
		Scenario:      name,
		Label:         o.label,
		StartedAt:     startedAt.UTC().Format(time.RFC3339Nano),
		Config:        map[string]any{"size_str": o.sizeStr, "keep_data": o.keepData},
		Result:        "pass",
		Steps:         []scenarioStep{},
	}

	switch name {
	case "cold-warm":
		err = runScenarioColdWarm(ctx, g, o, res)
	case "range-stress":
		err = runScenarioRangeStress(ctx, g, o, res)
	case "empty-object":
		err = runScenarioEmptyObject(ctx, g, o, res)
	case "etag-change":
		err = runScenarioETagChange(ctx, g, o, res)
	default:
		return fmt.Errorf("unknown scenario %q; one of: cold-warm, range-stress, empty-object, etag-change", name)
	}

	res.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		res.Result = "fail"
		res.FailureReason = err.Error()
	}

	if emitErr := emitScenarioResult(res, o); emitErr != nil {
		return emitErr
	}

	return err
}

// emitScenarioResult writes the human + JSON outputs.
func emitScenarioResult(res *scenarioResult, o *scenarioOpts) error {
	switch o.output {
	case "text":
		writeScenarioHuman(os.Stdout, res)
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
	}

	if o.jsonOut != "" {
		f, err := os.Create(o.jsonOut)
		if err != nil {
			return fmt.Errorf("create --json-out: %w", err)
		}

		defer f.Close() //nolint:errcheck

		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")

		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("write --json-out: %w", err)
		}
	}

	return nil
}

func writeScenarioHuman(w io.Writer, res *scenarioResult) {
	fprintf(w, "scenario: %s   label=%s   result=%s\n",
		res.Scenario, labelOrEmpty(res.Label), strings.ToUpper(res.Result))

	for _, s := range res.Steps {
		mark := "  OK "
		if !s.OK {
			mark = "  FAIL"
		}

		fprintf(w, "%s  %-20s  %s",
			mark, s.Name,
			time.Duration(s.ElapsedSeconds*float64(time.Second)).Round(time.Millisecond))

		if s.Error != "" {
			fprintf(w, "  err=%q", s.Error)
		}

		if len(s.Details) > 0 {
			fprintf(w, "  %s", formatScenarioDetails(s.Details))
		}

		fprintln(w)
	}

	if res.FailureReason != "" {
		fprintf(w, "\nfailure: %s\n", res.FailureReason)
	}
}

func formatScenarioDetails(d map[string]any) string {
	var parts []string

	for k, v := range d {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}

	return strings.Join(parts, " ")
}

// recordStep appends a step to the result and returns its elapsed
// duration for chained-step bookkeeping.
func recordStep(res *scenarioResult, name string, t0 time.Time, err error, details map[string]any) {
	step := scenarioStep{
		Name:           name,
		OK:             err == nil,
		ElapsedSeconds: time.Since(t0).Seconds(),
		Details:        details,
	}
	if err != nil {
		step.Error = err.Error()
	}

	res.Steps = append(res.Steps, step)
}

// recordDropCacheStep records the drop_cache step and returns err so
// the caller is forced to propagate failure rather than silently
// continuing into a false-positive "cold" GET. The function is named
// rather than inlined because the record-then-return-err contract is
// easy to break by accident when copy-pasting the cold-warm step
// pattern into a new scenario.
func recordDropCacheStep(res *scenarioResult, t0 time.Time, err error) error {
	recordStep(res, "drop_cache", t0, err, nil)

	return err
}

// --- cold-warm ---

func runScenarioColdWarm(ctx context.Context, g *globalFlags, o *scenarioOpts, res *scenarioResult) error {
	oc, err := newOriginClient(ctx, g)
	if err != nil {
		return err
	}

	if g.ensureContainer {
		if err := oc.EnsureBucket(ctx); err != nil {
			return err
		}
	}

	cs, err := newCachestoreClient(ctx, g)
	if err != nil {
		return err
	}

	edge := newEdgeClient(g.orcaURL, g.timeout)

	return runScenarioColdWarmWith(ctx, g, o, res, oc, cs, edge)
}

// runScenarioColdWarmWith runs the cold-warm scenario against an
// already-constructed origin client, cachestore, and edge client.
// Split out from runScenarioColdWarm so tests can drive the full
// step sequence (including failure paths like drop_cache returning
// an error) without standing up real Azure / S3 / orca backends.
func runScenarioColdWarmWith(
	ctx context.Context,
	g *globalFlags,
	o *scenarioOpts,
	res *scenarioResult,
	oc originClient,
	cs cachestoreOps,
	edge *edgeClient,
) error {
	size, err := parseSize(o.sizeStr)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}

	res.Config["size_bytes"] = size

	key := fmt.Sprintf("scenario-cold-warm-%d", time.Now().UnixNano())

	// Step 1: upload.
	t0 := time.Now()
	uploadErr := oc.Put(ctx, key, io.LimitReader(rand.Reader, size), size)
	recordStep(res, "upload", t0, uploadErr, map[string]any{"bytes": size, "key": key})

	if uploadErr != nil {
		return uploadErr
	}

	defer func() {
		if !o.keepData {
			_ = oc.Delete(ctx, key) //nolint:errcheck // cleanup best-effort
		}
	}()

	// Step 2: ensure cold cache by clearing any chunks for this key.
	t0 = time.Now()
	clearErr := clearScenarioObject(ctx, g, cs, oc, key, "", o.chunkSize)
	if err := recordDropCacheStep(res, t0, clearErr); err != nil {
		return err
	}

	// Step 3: cold GET.
	t0 = time.Now()
	coldBytes, coldErr := scenarioGet(ctx, edge, oc.Bucket(), key)
	coldElapsed := time.Since(t0)
	recordStep(res, "cold_get", t0, coldErr, map[string]any{"bytes": coldBytes})

	if coldErr != nil {
		return coldErr
	}

	// Step 4: warm GET (cached).
	t0 = time.Now()
	warmBytes, warmErr := scenarioGet(ctx, edge, oc.Bucket(), key)
	warmElapsed := time.Since(t0)
	recordStep(res, "warm_get", t0, warmErr, map[string]any{"bytes": warmBytes})

	if warmErr != nil {
		return warmErr
	}

	// Step 5: report speedup.
	ratio := 0.0
	if warmElapsed > 0 {
		ratio = float64(coldElapsed) / float64(warmElapsed)
	}

	recordStep(res, "speedup", time.Now(), nil, map[string]any{
		"cold_ms": coldElapsed.Milliseconds(),
		"warm_ms": warmElapsed.Milliseconds(),
		"ratio":   fmt.Sprintf("%.2f", ratio),
	})

	if coldBytes != size || warmBytes != size {
		return fmt.Errorf("byte count mismatch: cold=%d warm=%d want=%d", coldBytes, warmBytes, size)
	}

	return nil
}

// --- range-stress ---

func runScenarioRangeStress(ctx context.Context, g *globalFlags, o *scenarioOpts, res *scenarioResult) error {
	size, err := parseSize(o.sizeStr)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}

	res.Config["size_bytes"] = size

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

	key := fmt.Sprintf("scenario-range-stress-%d", time.Now().UnixNano())

	// Step 1: upload with known bytes and a known checksum.
	t0 := time.Now()
	source, sourceHash, sourceErr := scenarioSourceBuffer(size)
	if sourceErr != nil {
		recordStep(res, "upload", t0, sourceErr, map[string]any{"bytes": size})

		return sourceErr
	}

	uploadErr := oc.Put(ctx, key, bytes.NewReader(source), size)
	recordStep(res, "upload", t0, uploadErr, map[string]any{"bytes": size, "sha256": sourceHash})

	if uploadErr != nil {
		return uploadErr
	}

	defer func() {
		if !o.keepData {
			_ = oc.Delete(ctx, key) //nolint:errcheck
		}
	}()

	// Step 2: full GET, verify checksum.
	t0 = time.Now()

	resp, err := edge.Get(ctx, oc.Bucket(), key)
	if err != nil {
		recordStep(res, "full_get", t0, err, nil)
		return err
	}

	hh := hasher()
	n, err := io.Copy(hh, resp.Body)
	_ = resp.Body.Close() //nolint:errcheck
	recvHash := hexSum(hh)

	recordStep(res, "full_get", t0, err, map[string]any{
		"bytes":  n,
		"sha256": recvHash,
		"match":  recvHash == sourceHash,
	})

	if err != nil || recvHash != sourceHash {
		return fmt.Errorf("full GET hash mismatch (src=%s recv=%s)", sourceHash, recvHash)
	}

	// Step 3: multiple ranges, verify each.
	t0 = time.Now()

	const ranges = 8

	step := size / int64(ranges)

	var mismatch bool
	var rangeErr error

	for i := int64(0); i < int64(ranges); i++ {
		start := i * step
		end := start + step - 1

		if end >= size {
			end = size - 1
		}

		rresp, rerr := edge.GetRange(ctx, oc.Bucket(), key, start, end)
		if rerr != nil {
			mismatch = true
			rangeErr = rerr
			break
		}

		if err := verifyRangeResponse(rresp, source[start:end+1]); err != nil {
			mismatch = true
			rangeErr = err

			break
		}
	}

	recordStep(res, "ranges", t0, rangeErr, map[string]any{"count": ranges, "mismatch": mismatch})

	if mismatch {
		return fmt.Errorf("range fetch failed (one or more ranges did not return expected bytes)")
	}

	return nil
}

// scenarioRangeStressMaxSize caps the in-memory source buffer for
// the range-stress scenario. The verifier holds the full payload
// in RAM and each ranged GET response is buffered through
// io.ReadAll, so a 4 GiB ceiling keeps peak memory bounded at
// around 5 GiB on the host (source + one range read at a time)
// while still leaving headroom for serious load tests.
const scenarioRangeStressMaxSize = 4 * 1024 * 1024 * 1024

func scenarioSourceBuffer(size int64) ([]byte, string, error) {
	if size > scenarioRangeStressMaxSize {
		return nil, "", fmt.Errorf(
			"--size %s exceeds the range-stress in-memory ceiling of %s; "+
				"use bench or scale down --size",
			formatSize(size), formatSize(scenarioRangeStressMaxSize),
		)
	}

	if size > int64(math.MaxInt) {
		return nil, "", fmt.Errorf("--size %s exceeds addressable memory on this platform", formatSize(size))
	}

	if shouldLogRangeStressBufferNotice(size) {
		printErr("verifying: hashing %s source buffer for range comparison\n", formatSize(size))
	}

	buf := make([]byte, int(size))
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("generate source bytes: %w", err)
	}

	h := hasher()
	if _, err := h.Write(buf); err != nil {
		return nil, "", fmt.Errorf("hash source bytes: %w", err)
	}

	return buf, hexSum(h), nil
}

func shouldLogRangeStressBufferNotice(size int64) bool {
	return size > 1024*1024*1024
}

func verifyRangeResponse(resp edgeResponse, want []byte) error {
	defer resp.Body.Close() //nolint:errcheck

	if resp.Status != http.StatusPartialContent {
		return fmt.Errorf("range GET returned status %d, want %d", resp.Status, http.StatusPartialContent)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read range body: %w", err)
	}

	if !bytes.Equal(got, want) {
		offset := firstDiffOffset(want, got)
		// Align the hex window to the previous 16-byte boundary so
		// the dump is human-readable; cap at 64 bytes so a single
		// mismatch doesn't dump a megabyte of context.
		startOffset := offset - (offset % 16)
		if startOffset < 0 {
			startOffset = 0
		}

		return fmt.Errorf(
			"range body mismatch at offset %d: got %d bytes, want %d bytes\n%s",
			offset, len(got), len(want),
			hexDiffDump(want, got, startOffset, 64),
		)
	}

	return nil
}

// --- empty-object ---

func runScenarioEmptyObject(ctx context.Context, g *globalFlags, o *scenarioOpts, res *scenarioResult) error {
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

	key := fmt.Sprintf("scenario-empty-%d", time.Now().UnixNano())

	// Step 1: upload zero bytes.
	t0 := time.Now()
	uploadErr := oc.Put(ctx, key, strings.NewReader(""), 0)
	recordStep(res, "upload_empty", t0, uploadErr, map[string]any{"bytes": 0, "key": key})

	if uploadErr != nil {
		return uploadErr
	}

	defer func() {
		if !o.keepData {
			_ = oc.Delete(ctx, key) //nolint:errcheck
		}
	}()

	// Step 2: GET via orca; expect 200 + empty body.
	t0 = time.Now()
	resp, getErr := edge.Get(ctx, oc.Bucket(), key)

	var bytesRead int64
	if getErr == nil && resp.Body != nil {
		bytesRead, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck
		_ = resp.Body.Close()                         //nolint:errcheck
	}

	recordStep(res, "get_empty", t0, getErr, map[string]any{
		"status":     resp.Status,
		"bytes_read": bytesRead,
	})

	if getErr != nil {
		return getErr
	}

	if resp.Status != 200 {
		return fmt.Errorf("empty GET returned status %d, want 200", resp.Status)
	}

	if bytesRead != 0 {
		return fmt.Errorf("empty GET returned %d bytes, want 0", bytesRead)
	}

	return nil
}

// --- etag-change ---

func runScenarioETagChange(ctx context.Context, g *globalFlags, o *scenarioOpts, res *scenarioResult) error {
	size, err := parseSize(o.sizeStr)
	if err != nil {
		return fmt.Errorf("--size: %w", err)
	}

	res.Config["size_bytes"] = size

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

	key := fmt.Sprintf("scenario-etag-change-%d", time.Now().UnixNano())

	// v1 upload.
	t0 := time.Now()
	h1 := hasher()
	err = oc.Put(ctx, key, newTeeHashReader(io.LimitReader(rand.Reader, size), h1), size)
	hash1 := hexSum(h1)
	recordStep(res, "upload_v1", t0, err, map[string]any{"bytes": size, "sha256": hash1})

	if err != nil {
		return err
	}

	defer func() {
		if !o.keepData {
			_ = oc.Delete(ctx, key) //nolint:errcheck
		}
	}()

	// v1 GET via orca (populates cache).
	t0 = time.Now()

	v1Hash, _, _, _, err := fetchAndHash(ctx, edge, oc.Bucket(), key, "")
	recordStep(res, "get_v1", t0, err, map[string]any{"sha256": v1Hash, "match": v1Hash == hash1})

	if err != nil || v1Hash != hash1 {
		return fmt.Errorf("v1 GET hash mismatch (src=%s recv=%s)", hash1, v1Hash)
	}

	// Overwrite with v2 content (new etag).
	t0 = time.Now()
	h2 := hasher()
	err = oc.Put(ctx, key, newTeeHashReader(io.LimitReader(rand.Reader, size), h2), size)
	hash2 := hexSum(h2)
	recordStep(res, "upload_v2", t0, err, map[string]any{"bytes": size, "sha256": hash2})

	if err != nil {
		return err
	}

	// v2 GET via orca. Orca's HEAD-with-cached-ETag path should
	// detect the etag change and either revalidate or return v2.
	// We accept either v2 bytes (most likely) or an
	// OriginETagChanged 502 (acceptable: orca refused to serve
	// stale bytes).
	t0 = time.Now()

	v2Hash, status, _, _, err := fetchAndHash(ctx, edge, oc.Bucket(), key, "")
	recordStep(res, "get_v2", t0, err, map[string]any{
		"status": status,
		"sha256": v2Hash,
		"match":  v2Hash == hash2,
	})

	if err != nil {
		return err
	}

	switch status {
	case 200:
		if v2Hash != hash2 {
			return fmt.Errorf("v2 GET returned 200 but hash %s != v2 %s (stale serve)", v2Hash, hash2)
		}
	case 502:
		// Acceptable: orca detected the change and refused to
		// serve. The scenario does not enforce one outcome.
	default:
		return fmt.Errorf("v2 GET returned status %d", status)
	}

	return nil
}

// --- helpers ---

// scenarioGet issues a full GET and returns bytes read (discarded).
func scenarioGet(ctx context.Context, edge *edgeClient, bucket, key string) (int64, error) {
	resp, err := edge.Get(ctx, bucket, key)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close() //nolint:errcheck

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return n, err
	}

	if resp.Status != 200 {
		return n, fmt.Errorf("status %d", resp.Status)
	}

	return n, nil
}

// clearScenarioObject removes cached chunks for the given object so
// the next GET is forced to refill from origin. Used by the
// cold-warm scenario.
func clearScenarioObject(ctx context.Context, g *globalFlags, cs cachestoreOps, oc originClient, key, etag, chunkSizeOverride string) error {
	chunkSize, err := resolveChunkSize(g, chunkSizeOverride)
	if err != nil {
		return err
	}

	if etag == "" {
		info, err := oc.Head(ctx, key)
		if err != nil {
			return err
		}

		etag = info.ETag
	}

	// Walk a bounded index range deleting any chunks that exist.
	for i := int64(0); i < 1024; i++ {
		k := chunk.Key{
			OriginID:  g.originID,
			Bucket:    oc.Bucket(),
			ObjectKey: key,
			ETag:      etag,
			ChunkSize: chunkSize,
			Index:     i,
		}
		path := k.Path()

		_, herr := cs.Head(ctx, path)
		if herr != nil {
			// not found at this index = no more chunks for this
			// object (chunk paths are sequential).
			break
		}

		if err := cs.Delete(ctx, path); err != nil {
			return err
		}
	}

	return nil
}
