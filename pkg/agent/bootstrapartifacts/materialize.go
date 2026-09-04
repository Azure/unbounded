// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

const httpsArchiveReadyMarker = ".ready"

type materializedHTTPSArchive struct {
	root string
}

func (a *materializedHTTPSArchive) markValidated() error {
	if a == nil || a.root == "" {
		return errors.New("materialized HTTPS archive is not initialized")
	}

	path := filepath.Join(a.root, httpsArchiveReadyMarker)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("mark HTTPS archive validated: %w", err)
	}

	return nil
}

func materializeHTTPSArchive(ctx context.Context, source artifactsource.Source, storageRoot string) (*materializedHTTPSArchive, error) {
	if storageRoot == "" {
		return nil, errors.New("HTTPS archive storage root is required")
	}

	archive := &materializedHTTPSArchive{
		root: filepath.Join(storageRoot, SourceKey(source.String())),
	}
	if archive.isValidated() {
		return archive, nil
	}

	if err := os.RemoveAll(archive.root); err != nil {
		return nil, fmt.Errorf("remove incomplete HTTPS archive: %w", err)
	}

	if err := os.MkdirAll(storageRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create HTTPS archive storage root: %w", err)
	}

	tempDir, err := os.MkdirTemp(storageRoot, ".extract-")
	if err != nil {
		return nil, fmt.Errorf("create HTTPS archive extraction directory: %w", err)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // best effort cleanup

	if err := source.ExtractTar(ctx, tempDir); err != nil {
		return nil, fmt.Errorf("download and extract HTTPS archive: %w", err)
	}

	archiveRoot, err := findArchiveRoot(tempDir)
	if err != nil {
		return nil, err
	}

	if _, err := os.Lstat(filepath.Join(archiveRoot, httpsArchiveReadyMarker)); err == nil {
		return nil, fmt.Errorf("HTTPS archive contains reserved file %q", httpsArchiveReadyMarker)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect HTTPS archive validation marker: %w", err)
	}

	if err := os.Rename(archiveRoot, archive.root); err != nil {
		if archive.isValidated() {
			return archive, nil
		}

		return nil, fmt.Errorf("install HTTPS archive: %w", err)
	}

	return archive, nil
}

// SourceKey returns a filesystem-safe source identity key. Signed HTTP(S)
// query parameters are excluded so credential rotation reuses the same key.
func SourceKey(source string) string {
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

func (a *materializedHTTPSArchive) isValidated() bool {
	info, err := os.Stat(filepath.Join(a.root, httpsArchiveReadyMarker))

	return err == nil && info.Mode().IsRegular()
}

func findArchiveRoot(extractDir string) (string, error) {
	var markers []string

	if err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() == ManifestFileName {
			markers = append(markers, path)
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("inspect extracted HTTPS archive: %w", err)
	}

	if len(markers) == 0 {
		return "", fmt.Errorf("HTTPS archive does not contain %s", ManifestFileName)
	}

	if len(markers) > 1 {
		return "", fmt.Errorf("HTTPS archive contains multiple %s files", ManifestFileName)
	}

	return filepath.Dir(markers[0]), nil
}
