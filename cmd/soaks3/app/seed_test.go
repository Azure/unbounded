// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func baseSeedOpts(dir string) *seedOptions {
	return &seedOptions{
		outDir:      dir,
		objectSize:  "1KiB",
		keyPrefix:   "soaks3/",
		count:       8,
		concurrency: 4,
		seed:        1,
	}
}

func TestRunSeedWritesObjectsAndManifest(t *testing.T) {
	dir := t.TempDir()
	opts := baseSeedOpts(dir)

	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	km, _ := newKeyModel(opts.keyPrefix, opts.count)

	for i := int64(0); i < opts.count; i++ {
		p := filepath.Join(dir, km.relPathForIndex(i))

		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("object %d missing: %v", i, err)
		}

		if info.Size() != 1024 {
			t.Errorf("object %d size = %d, want 1024", i, info.Size())
		}
	}

	man, err := readManifest(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	if man.Count != 8 || man.ObjectSize != 1024 || man.KeyPrefix != "soaks3/" || man.Seed != 1 {
		t.Errorf("unexpected manifest: %+v", man)
	}
}

func TestRunSeedRequiresOutDir(t *testing.T) {
	opts := baseSeedOpts("")
	if err := runSeed(context.Background(), opts); err == nil {
		t.Fatal("expected error for missing --out-dir")
	}
}

func TestRunSeedMutualExclusionFlags(t *testing.T) {
	opts := baseSeedOpts(t.TempDir())
	opts.overwrite = true
	opts.skipExisting = true

	if err := runSeed(context.Background(), opts); err == nil {
		t.Fatal("expected error for --overwrite with --skip-existing")
	}
}

func TestRunSeedCountAndTotalSizeExclusive(t *testing.T) {
	opts := baseSeedOpts(t.TempDir())
	opts.totalSize = "1MiB"

	if err := runSeed(context.Background(), opts); err == nil {
		t.Fatal("expected error for --count with --total-size")
	}
}

func TestRunSeedTotalSize(t *testing.T) {
	dir := t.TempDir()
	opts := &seedOptions{
		outDir:      dir,
		objectSize:  "1KiB",
		keyPrefix:   "soaks3/",
		totalSize:   "4KiB",
		concurrency: 2,
		seed:        1,
	}

	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	man, err := readManifest(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	if man.Count != 4 {
		t.Errorf("count = %d, want 4", man.Count)
	}
}

func TestSeedCommandDefaults(t *testing.T) {
	cmd := newSeedCommand()

	objectSize, err := cmd.Flags().GetString("object-size")
	if err != nil {
		t.Fatal(err)
	}

	totalSize, err := cmd.Flags().GetString("total-size")
	if err != nil {
		t.Fatal(err)
	}

	if objectSize != "1.25GB" {
		t.Errorf("default object-size = %q, want 1.25GB", objectSize)
	}

	if totalSize != "10GB" {
		t.Errorf("default total-size = %q, want 10GB", totalSize)
	}

	// The defaults should derive a whole number of objects.
	os, err := parseSize(objectSize)
	if err != nil {
		t.Fatal(err)
	}

	ts, err := parseSize(totalSize)
	if err != nil {
		t.Fatal(err)
	}

	count, err := deriveCount(0, ts, os)
	if err != nil {
		t.Fatal(err)
	}

	if count != 8 {
		t.Errorf("default count = %d, want 8", count)
	}
}

func TestSeedCommandCountOverridesDefaultTotalSize(t *testing.T) {
	dir := t.TempDir()

	cmd := newSeedCommand()
	cmd.SetArgs([]string{
		"--out-dir", dir,
		"--count", "4",
		"--object-size", "1KiB",
		"--concurrency", "2",
	})

	// An explicit --count must win over the defaulted --total-size rather
	// than tripping the mutual-exclusion check.
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("seed with explicit --count failed: %v", err)
	}

	man, err := readManifest(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	if man.Count != 4 {
		t.Errorf("count = %d, want 4", man.Count)
	}
}

func TestRunSeedExistingWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	opts := baseSeedOpts(dir)

	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	// Second run without --overwrite or --skip-existing should fail.
	if err := runSeed(context.Background(), opts); err == nil {
		t.Fatal("expected error re-seeding existing objects")
	}
}

func TestRunSeedSkipExisting(t *testing.T) {
	dir := t.TempDir()
	opts := baseSeedOpts(dir)

	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	opts.skipExisting = true
	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatalf("skip-existing rerun failed: %v", err)
	}
}

func TestRunSeedOverwrite(t *testing.T) {
	dir := t.TempDir()
	opts := baseSeedOpts(dir)

	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	opts.overwrite = true
	if err := runSeed(context.Background(), opts); err != nil {
		t.Fatalf("overwrite rerun failed: %v", err)
	}
}

func TestContentReaderDeterministic(t *testing.T) {
	read := func() []byte {
		r := newContentReader(7, 3, 5000)

		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}

		return b
	}

	a := read()
	b := read()

	if len(a) != 5000 {
		t.Fatalf("content length = %d, want 5000", len(a))
	}

	if !bytes.Equal(a, b) {
		t.Fatal("content reader not deterministic")
	}
}

func TestContentReaderDistinctPerObject(t *testing.T) {
	r0, _ := io.ReadAll(newContentReader(1, 0, 1024))
	r1, _ := io.ReadAll(newContentReader(1, 1, 1024))

	if bytes.Equal(r0, r1) {
		t.Fatal("expected different content for different object indices")
	}
}

func TestContentReaderExactSize(t *testing.T) {
	for _, size := range []int64{0, 1, 7, 8, 9, 1000} {
		b, err := io.ReadAll(newContentReader(1, 0, size))
		if err != nil {
			t.Fatal(err)
		}

		if int64(len(b)) != size {
			t.Errorf("size %d: got %d bytes", size, len(b))
		}
	}
}
