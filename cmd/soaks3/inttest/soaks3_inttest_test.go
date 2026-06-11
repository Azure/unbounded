// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

// Package inttest holds the soaks3 integration test. It spins up a
// single-node Garage S3 backend, seeds a small deterministic data set,
// uploads it to a bucket, and then drives a short read-load pass with
// the real soaks3 binary directly against Garage's anonymous web
// endpoint. The point is not to benchmark anything meaningful but to
// prove the tool builds, talks to a live S3-compatible store, and emits
// coherent output (a healthy count of successful requests, negligible
// errors).
package inttest

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// seedCount is the number of objects seeded and uploaded. Small enough
// that seeding and uploading finish quickly, large enough that the Zipf
// selector has a real key space to draw from.
const seedCount = 32

// objectSize is the per-object size for the seeded data set. Kept tiny
// so the upload and the load pass stay fast in CI.
const objectSize = "256KiB"

// loadDuration is how long the run pass drives load. A few seconds is
// plenty to accumulate thousands of requests against a local container.
const loadDuration = 3 * time.Second

var (
	// summaryRequestsRe matches the request total in the run summary,
	// e.g. "[soaks3] requests:   12345 (4115 req/s)".
	summaryRequestsRe = regexp.MustCompile(`(?m)^\[soaks3\] requests:\s+(\d+)`)
	// summaryErrorsRe matches the error total in the run summary, e.g.
	// "[soaks3] errors:     0 (0.00%)".
	summaryErrorsRe = regexp.MustCompile(`(?m)^\[soaks3\] errors:\s+(\d+)`)
)

// TestSoaks3AgainstGarage is the end-to-end smoke test: seed -> upload
// -> run against a live Garage, asserting the run produces a coherent
// summary with successful requests and no errors.
func TestSoaks3AgainstGarage(t *testing.T) {
	ctx := context.Background()

	garage, err := StartGarage(ctx)
	if err != nil {
		t.Fatalf("start garage: %v", err)
	}

	t.Cleanup(func() {
		_ = garage.Terminate(context.Background()) //nolint:errcheck // best-effort cleanup
	})

	bucket := garage.PrepareBucket(ctx, t)
	bin := soaks3Binary(t)

	// Seed a deterministic data set onto the local filesystem.
	seedDir := t.TempDir()
	seedOut := runSoaks3(
		ctx, t, bin, 60*time.Second,
		"seed",
		"--out-dir", seedDir,
		"--count", strconv.Itoa(seedCount),
		"--object-size", objectSize,
		"--concurrency", "4",
	)
	t.Logf("seed output:\n%s", seedOut)

	// Upload the seeded tree into the bucket so the run pass has objects
	// to read back. seed writes paths that mirror the bucket key layout
	// exactly, so the relative path is the S3 key.
	uploadSeedDir(ctx, t, garage, bucket, seedDir)

	// Drive a short read-load pass directly against Garage's anonymous
	// web endpoint. --bucket is left empty so the generated URLs are
	// path-style against the web endpoint (the bucket is selected by the
	// Host alias). The metrics server is disabled (we assert on stdout)
	// and the duration keeps the pass bounded.
	runOut := runSoaks3(
		ctx, t, bin, loadDuration+60*time.Second,
		"run",
		"--endpoint", garage.WebEndpoint(),
		"--bucket", "",
		"--manifest", filepath.Join(seedDir, "manifest.json"),
		"--duration", loadDuration.String(),
		"--concurrency", "4",
		"--metrics-addr", "",
		"--report-interval", "1s",
	)
	t.Logf("run output:\n%s", runOut)

	if !strings.Contains(runOut, "---- summary ----") {
		t.Fatalf("run output missing summary section:\n%s", runOut)
	}

	requests := mustMatchInt(t, summaryRequestsRe, runOut, "requests")
	if requests <= 0 {
		t.Fatalf("expected a positive request count, got %d:\n%s", requests, runOut)
	}

	// A handful of requests in flight when the duration elapses get
	// cancelled during shutdown, so don't insist on exactly zero. The
	// point is that the vast majority succeeded: require the error rate
	// to stay well under 5%, which proves soaks3 is genuinely reading
	// objects rather than, say, 403ing on every request.
	errCount := mustMatchInt(t, summaryErrorsRe, runOut, "errors")
	if errCount*20 >= requests {
		t.Fatalf("error rate too high: %d errors of %d requests:\n%s", errCount, requests, runOut)
	}
}

// uploadSeedDir walks the seed output tree and PutObjects every seeded
// object into the bucket. The manifest file lives at the tree root and
// is not an object, so it is skipped. The object key is the path
// relative to seedDir (forward-slashed), which is exactly the key the
// run pass will request.
func uploadSeedDir(ctx context.Context, t *testing.T, garage *Garage, bucket, seedDir string) {
	t.Helper()

	cli := garage.s3Client(ctx, t)

	var uploaded int

	err := filepath.WalkDir(seedDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(seedDir, path)
		if err != nil {
			return err
		}

		if rel == "manifest.json" {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		key := filepath.ToSlash(rel)

		if _, err := cli.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("put object %s: %v", key, err)
		}

		uploaded++

		return nil
	})
	if err != nil {
		t.Fatalf("walk seed dir: %v", err)
	}

	if uploaded != seedCount {
		t.Fatalf("expected to upload %d objects, uploaded %d", seedCount, uploaded)
	}
}

// soaks3Binary returns the path to a soaks3 binary to exercise. When the
// caller (e.g. the Makefile target) sets SOAKS3_BIN, that prebuilt
// binary is used; otherwise the binary is built on the fly so the test
// is self-contained under `go test`.
func soaks3Binary(t *testing.T) string {
	t.Helper()

	if bin := os.Getenv("SOAKS3_BIN"); bin != "" {
		return bin
	}

	bin := filepath.Join(t.TempDir(), "soaks3")

	cmd := exec.Command("go", "build", "-o", bin, "github.com/Azure/unbounded/cmd/soaks3")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build soaks3: %v\n%s", err, out)
	}

	return bin
}

// runSoaks3 runs the soaks3 binary with args under a timeout and returns
// its combined output, failing the test on error.
func runSoaks3(ctx context.Context, t *testing.T, bin string, timeout time.Duration, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("soaks3 %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}

// mustMatchInt extracts the first capture group of re from out and
// parses it as an int, failing the test if absent or unparseable.
func mustMatchInt(t *testing.T, re *regexp.Regexp, out, label string) int {
	t.Helper()

	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not find %s in output:\n%s", label, out)
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse %s %q: %v", label, m[1], err)
	}

	return n
}
