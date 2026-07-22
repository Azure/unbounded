// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExtractTar extracts a tar or gzip-compressed tar stream into destDir.
// Only directories and regular files are accepted.
func ExtractTar(body io.Reader, destDir string) error {
	buffered := bufio.NewReader(body)

	archiveReader, closeArchive, err := tarArchiveReader(buffered)
	if err != nil {
		return err
	}
	defer closeArchive.Close() //nolint:errcheck // best effort close

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create archive destination %q: %w", destDir, err)
	}

	seen := map[string]struct{}{}
	tarReader := tar.NewReader(archiveReader)

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read tar archive: %w", err)
		}

		if header.Typeflag == tar.TypeDir && (header.Name == "." || header.Name == "./") {
			continue
		}

		name, err := cleanedTarEntryName(header.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry %q: %w", header.Name, err)
		}

		path := filepath.Join(destDir, name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return fmt.Errorf("create archive directory %q: %w", name, err)
			}
		case tar.TypeReg, 0:
			if _, ok := seen[name]; ok {
				return fmt.Errorf("duplicate tar entry %q", name)
			}

			seen[name] = struct{}{}

			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create parent for archive file %q: %w", name, err)
			}

			if err := writeTarFile(path, tarReader); err != nil {
				return fmt.Errorf("extract archive file %q: %w", name, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry type %d for %q", header.Typeflag, name)
		}
	}
}

func tarArchiveReader(buffered *bufio.Reader) (io.Reader, io.Closer, error) {
	magic, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("inspect archive compression: %w", err)
	}

	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip archive: %w", err)
		}

		return gzipReader, gzipReader, nil
	}

	return buffered, nopCloser{}, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func writeTarFile(path string, body io.Reader) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()

	if _, err := io.Copy(file, body); err != nil {
		return err
	}

	return nil
}
