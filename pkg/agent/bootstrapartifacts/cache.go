// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// ArchiveCacheOptions configures extraction of an archive source into a
// source-specific cache directory.
type ArchiveCacheOptions struct {
	CacheRoot   string
	RootMarker  string
	ReadyMarker string
}

// ArchiveCache is an extracted archive cache that can be marked ready after
// caller-specific validation succeeds.
type ArchiveCache struct {
	root        string
	readyMarker string
}

// Root returns the extracted archive root.
func (c *ArchiveCache) Root() string {
	if c == nil {
		return ""
	}

	return c.root
}

// MarkReady marks an extracted archive cache as validated and reusable.
func (c *ArchiveCache) MarkReady() error {
	if c == nil || c.root == "" || c.readyMarker == "" {
		return errors.New("archive cache is not initialized")
	}

	path := filepath.Join(c.root, c.readyMarker)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("mark archive cache ready: %w", err)
	}

	return nil
}

// MaterializeArchive downloads and extracts an archive source into a stable
// source-specific cache. The returned cache must be marked ready after the
// caller validates its contents.
func MaterializeArchive(ctx context.Context, source artifactsource.Source, opts ArchiveCacheOptions) (*ArchiveCache, error) {
	if err := validateArchiveCacheOptions(opts); err != nil {
		return nil, err
	}

	cache := &ArchiveCache{
		root:        filepath.Join(opts.CacheRoot, CacheKey(source.String())),
		readyMarker: opts.ReadyMarker,
	}
	if cache.isReady() {
		return cache, nil
	}

	if err := os.RemoveAll(cache.root); err != nil {
		return nil, fmt.Errorf("remove incomplete archive cache: %w", err)
	}

	if err := os.MkdirAll(opts.CacheRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create archive cache root: %w", err)
	}

	tempDir, err := os.MkdirTemp(opts.CacheRoot, ".extract-")
	if err != nil {
		return nil, fmt.Errorf("create archive extraction directory: %w", err)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // best effort cleanup

	if err := source.ExtractTar(ctx, tempDir); err != nil {
		return nil, fmt.Errorf("download and extract archive: %w", err)
	}

	archiveRoot, err := findArchiveRoot(tempDir, opts.RootMarker)
	if err != nil {
		return nil, err
	}

	if _, err := os.Lstat(filepath.Join(archiveRoot, opts.ReadyMarker)); err == nil {
		return nil, fmt.Errorf("archive contains reserved file %q", opts.ReadyMarker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect archive cache marker: %w", err)
	}

	if err := os.Rename(archiveRoot, cache.root); err != nil {
		if cache.isReady() {
			return cache, nil
		}

		return nil, fmt.Errorf("install archive cache: %w", err)
	}

	return cache, nil
}

// CacheKey returns a filesystem-safe, query-independent source cache key.
func CacheKey(source string) string {
	sourceIdentity := utilio.URLWithoutQuery(source)

	var prefixBuilder strings.Builder

	for _, r := range strings.ToLower(sourceIdentity) {
		switch {
		case r >= 'a' && r <= 'z':
			prefixBuilder.WriteRune(r)
		case r >= '0' && r <= '9':
			prefixBuilder.WriteRune(r)
		case r == '.', r == '_', r == '-':
			prefixBuilder.WriteRune(r)
		default:
			prefixBuilder.WriteByte('-')
		}
	}

	prefix := strings.Trim(prefixBuilder.String(), "-")
	if len(prefix) > 80 {
		prefix = strings.TrimRight(prefix[:80], "-")
	}

	if prefix == "" {
		prefix = "source"
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(sourceIdentity)))[:12]

	return prefix + "-" + hash
}

func (c *ArchiveCache) isReady() bool {
	info, err := os.Stat(filepath.Join(c.root, c.readyMarker))

	return err == nil && info.Mode().IsRegular()
}

func validateArchiveCacheOptions(opts ArchiveCacheOptions) error {
	if opts.CacheRoot == "" {
		return errors.New("archive cache root is required")
	}

	if opts.RootMarker == "" || filepath.Base(opts.RootMarker) != opts.RootMarker {
		return errors.New("archive root marker must be a file name")
	}

	if opts.ReadyMarker == "" || filepath.Base(opts.ReadyMarker) != opts.ReadyMarker {
		return errors.New("archive ready marker must be a file name")
	}

	return nil
}

func findArchiveRoot(extractDir, rootMarker string) (string, error) {
	var markers []string

	if err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() == rootMarker {
			markers = append(markers, path)
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("inspect extracted archive: %w", err)
	}

	if len(markers) == 0 {
		return "", fmt.Errorf("archive does not contain %s", rootMarker)
	}

	if len(markers) > 1 {
		return "", fmt.Errorf("archive contains multiple %s files", rootMarker)
	}

	return filepath.Dir(markers[0]), nil
}
