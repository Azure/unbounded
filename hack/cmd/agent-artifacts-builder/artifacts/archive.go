// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"
)

// WriteBundleArchive writes rootDir as a deterministic gzip-compressed tar
// archive and writes an adjacent sha256sum file.
func WriteBundleArchive(rootDir, archivePath string) error {
	if rootDir == "" {
		return fmt.Errorf("bundle root directory is required")
	}

	if archivePath == "" {
		return fmt.Errorf("bundle archive path is required")
	}

	return writeDirectoryArchive(rootDir, archivePath)
}

func writeDirectoryArchive(rootDir, archivePath string) error {
	inside, err := pathIsWithin(rootDir, archivePath)
	if err != nil {
		return err
	}

	if inside {
		return fmt.Errorf("bundle archive %q must be outside bundle root %q", archivePath, rootDir)
	}

	paths, err := collectArtifactPaths(rootDir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return fmt.Errorf("create bundle archive directory: %w", err)
	}

	pending, err := renameio.NewPendingFile(archivePath, renameio.WithPermissions(0o644))
	if err != nil {
		return fmt.Errorf("create bundle archive %q: %w", archivePath, err)
	}
	defer pending.Cleanup() //nolint:errcheck // pending file cleanup

	gzipWriter := gzip.NewWriter(pending)
	gzipWriter.ModTime = time.Unix(0, 0)
	gzipWriter.OS = 255

	tarWriter := tar.NewWriter(gzipWriter)

	if err := writeBundleArchiveEntries(tarWriter, rootDir, paths); err != nil {
		return err
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("close bundle tar archive: %w", err)
	}

	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("close bundle gzip archive: %w", err)
	}

	if err := pending.CloseAtomicallyReplace(); err != nil {
		return fmt.Errorf("install bundle archive %q: %w", archivePath, err)
	}

	if err := writeGeneratedChecksum(archivePath); err != nil {
		return fmt.Errorf("write bundle archive checksum: %w", err)
	}

	return nil
}

func writeBundleArchiveEntries(writer *tar.Writer, rootDir string, paths []string) error {
	for _, path := range paths {
		fullPath := filepath.Join(rootDir, filepath.FromSlash(path))

		info, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("stat bundle archive entry %q: %w", path, err)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundle archive entry %q is not a regular file", path)
		}

		header := &tar.Header{
			Name:     filepath.ToSlash(path),
			Mode:     0o644,
			Size:     info.Size(),
			ModTime:  time.Unix(0, 0),
			Typeflag: tar.TypeReg,
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write bundle archive header %q: %w", path, err)
		}

		file, err := os.Open(fullPath)
		if err != nil {
			return fmt.Errorf("open bundle archive entry %q: %w", path, err)
		}

		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()

		if copyErr != nil {
			return fmt.Errorf("write bundle archive entry %q: %w", path, copyErr)
		}

		if closeErr != nil {
			return fmt.Errorf("close bundle archive entry %q: %w", path, closeErr)
		}
	}

	return nil
}

func pathIsWithin(root, path string) (bool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("resolve bundle root %q: %w", root, err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve bundle archive path %q: %w", path, err)
	}

	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false, fmt.Errorf("compare bundle archive path to root: %w", err)
	}

	inside := rel == "." || (rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))

	return inside, nil
}
