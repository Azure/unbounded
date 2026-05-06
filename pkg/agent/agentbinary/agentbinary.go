// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package agentbinary installs unbounded-agent binaries from release archives.
package agentbinary

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// InstallFromTarGz downloads a remote .tar.gz archive and installs binaryName
// from it to targetPath.
func InstallFromTarGz(ctx context.Context, downloadURL, targetPath, binaryName string, perm os.FileMode) error {
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		return fmt.Errorf("parse download URL %q: %w", downloadURL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("unsupported agent download URL scheme %q", parsedURL.Scheme)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return err
		}
		if filepath.Base(tarFile.Name) != binaryName {
			continue
		}

		countingReader := &countingReader{r: tarFile.Body}
		if err := utilio.InstallFile(targetPath, countingReader, perm); err != nil {
			return fmt.Errorf("install %s from %q: %w", binaryName, downloadURL, err)
		}
		if countingReader.n == 0 {
			return fmt.Errorf("agent binary %q in archive %q is empty", binaryName, downloadURL)
		}

		return nil
	}

	return fmt.Errorf("agent binary %q not found in archive %q", binaryName, downloadURL)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)

	return n, err
}
