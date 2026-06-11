// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// manifestName is the file written at the seed output root describing the
// generated data set, so a subsequent run can auto-configure itself.
const manifestName = "manifest.json"

// manifest describes a seeded data set.
type manifest struct {
	Count      int64  `json:"count"`
	ObjectSize int64  `json:"objectSize"`
	KeyPrefix  string `json:"keyPrefix"`
	Seed       int64  `json:"seed"`
}

// writeManifest writes the manifest as JSON to path.
func writeManifest(path string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest %s: %w", path, err)
	}

	return nil
}

// readManifest loads a manifest from path.
func readManifest(path string) (manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest %s: %w", path, err)
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	return m, nil
}

// indexWidth is the zero-padded width of the numeric object index in a key.
// 10 digits supports up to 10 billion objects while keeping keys fixed-width.
const indexWidth = 10

// keyModel maps a contiguous range of object indices [0, count) to
// deterministic S3 keys and filesystem-relative paths. The mapping is
// identical across every soaks3 instance so that a seeded data set and the
// read load generated against it agree on key names without coordination.
type keyModel struct {
	// prefix is prepended to every key. It is normalized to end with a single
	// trailing slash (unless empty). Example: "soaks3/".
	prefix string
	// count is the number of objects in the data set.
	count int64
}

// newKeyModel builds a keyModel from a key prefix and an object count.
func newKeyModel(prefix string, count int64) (keyModel, error) {
	if count < 0 {
		return keyModel{}, fmt.Errorf("count must not be negative, got %d", count)
	}

	return keyModel{prefix: normalizePrefix(prefix), count: count}, nil
}

// normalizePrefix trims leading slashes and collapses the trailing slash so
// that the prefix joins cleanly with the per-object suffix.
func normalizePrefix(prefix string) string {
	p := strings.TrimSpace(prefix)
	p = strings.TrimLeft(p, "/")

	if p == "" {
		return ""
	}

	return strings.TrimRight(p, "/") + "/"
}

// keyForIndex returns the S3 key for the object at index i, for example
// "soaks3/obj-0000000123". It does not bounds-check i against count so callers
// may use it for arbitrary indices selected by a distribution.
func (m keyModel) keyForIndex(i int64) string {
	return fmt.Sprintf("%sobj-%0*d", m.prefix, indexWidth, i)
}

// relPathForIndex returns the filesystem-relative path for the object at index
// i, mirroring the key structure so the on-disk tree maps 1:1 onto bucket keys.
func (m keyModel) relPathForIndex(i int64) string {
	return filepath.FromSlash(m.keyForIndex(i))
}

// deriveCount resolves the object count from mutually exclusive count and
// totalSize inputs. Exactly one of count or totalSize must be non-zero.
// objectSize must be positive whenever totalSize is used.
func deriveCount(count, totalSize, objectSize int64) (int64, error) {
	switch {
	case count < 0:
		return 0, fmt.Errorf("--count must not be negative, got %d", count)
	case totalSize < 0:
		return 0, fmt.Errorf("--total-size must not be negative, got %d", totalSize)
	case count > 0 && totalSize > 0:
		return 0, fmt.Errorf("--count and --total-size are mutually exclusive")
	case count == 0 && totalSize == 0:
		return 0, fmt.Errorf("one of --count or --total-size must be set")
	case count > 0:
		return count, nil
	}

	if objectSize <= 0 {
		return 0, fmt.Errorf("--object-size must be positive when using --total-size, got %d", objectSize)
	}

	n := totalSize / objectSize
	if n == 0 {
		return 0, fmt.Errorf("--total-size %d is smaller than --object-size %d", totalSize, objectSize)
	}

	return n, nil
}
