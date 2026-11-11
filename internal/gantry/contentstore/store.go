// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package contentstore gives read-only access to blobs in containerd's local
// content store. Containerd commits a blob under its digest only after
// verifying the bytes, so a blob found here is trusted as content-addressed
// and callers do not re-hash it.
package contentstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot is containerd's default content store directory.
const DefaultRoot = "/var/lib/containerd/io.containerd.content.v1.content"

// ErrNotFound reports that the blob is not present in the content store.
var ErrNotFound = errors.New("contentstore: blob not found")

// ErrInvalidDigest reports a digest that is not a canonical sha256 digest.
var ErrInvalidDigest = errors.New("contentstore: invalid digest")

// Store opens blobs beneath a containerd content store root.
type Store struct {
	root string
}

// New returns a Store for root, which must be an absolute, clean path. The
// root need not exist yet; missing blobs report ErrNotFound.
func New(root string) (*Store, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("contentstore: root must be an absolute clean path, got %q", root)
	}

	return &Store{root: root}, nil
}

// Root returns the configured content store root.
func (s *Store) Root() string { return s.root }

// Open opens the blob for digest d ("sha256:<64 lowercase hex>") read-only and
// returns it with its size. Symlinks are never followed and the result must be
// a regular file. A missing blob, or a non-regular entry, reports ErrNotFound.
func (s *Store) Open(d string) (*os.File, int64, error) {
	hex, err := parseDigest(d)
	if err != nil {
		return nil, 0, err
	}

	f, err := openBeneath(s.root, filepath.Join("blobs", "sha256", hex))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isSymlinkError(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, d)
		}

		return nil, 0, fmt.Errorf("contentstore: open %s: %w", d, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close() //nolint:errcheck // best-effort close after failed stat

		return nil, 0, fmt.Errorf("contentstore: stat %s: %w", d, err)
	}

	if !info.Mode().IsRegular() {
		_ = f.Close() //nolint:errcheck // best-effort close of a non-regular entry

		return nil, 0, fmt.Errorf("%w: %s is not a regular file", ErrNotFound, d)
	}

	return f, info.Size(), nil
}

func parseDigest(d string) (string, error) {
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 {
		return "", fmt.Errorf("%w: %q", ErrInvalidDigest, d)
	}

	for i := range len(hex) {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: %q", ErrInvalidDigest, d)
		}
	}

	return hex, nil
}

// openRoot opens rel beneath root with os.Root, which refuses to escape the
// root, and O_NOFOLLOW so a symlinked final component is refused. O_NONBLOCK
// keeps a FIFO with no writer from blocking the open before Open rejects it
// as non-regular; it has no effect on regular files.
func openRoot(root, rel string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close() //nolint:errcheck // read-only directory handle

	return r.OpenFile(rel, os.O_RDONLY|noFollow|nonBlock, 0)
}
