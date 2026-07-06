// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

func downloadToLocalFile(ctx context.Context, source, filename string, perm os.FileMode) error {
	if isOCIDownloadSource(source) {
		return ociartifact.DownloadToLocalFile(ctx, source, filename, perm)
	}

	return utilio.DownloadToLocalFile(ctx, source, filename, perm)
}

func downloadWithSHA256Verification(ctx context.Context, source, checksumSource, filename string, perm os.FileMode) error {
	if isOCIDownloadSource(source) || isOCIDownloadSource(checksumSource) {
		return ociartifact.DownloadWithSHA256Verification(ctx, source, checksumSource, filename, perm)
	}

	return utilio.DownloadWithSHA256Verification(ctx, source, checksumSource, filename, perm)
}

func decompressTarGzFromRemote(ctx context.Context, source string) utilio.TarFileSeq {
	if isOCIDownloadSource(source) {
		return ociartifact.DecompressTarGzFromRemote(ctx, source)
	}

	return utilio.DecompressTarGzFromRemote(ctx, source)
}

func probeArtifactObject(ctx context.Context, source string) error {
	if isOCIDownloadSource(source) {
		return ociartifact.Probe(ctx, source)
	}

	if isLocalDownloadSource(source) {
		return probeLocalObject(source)
	}

	return utilio.ProbeRemoteHTTPObject(ctx, source)
}

func isOCIDownloadSource(source string) bool {
	return strings.HasPrefix(source, "oci://")
}

func isLocalDownloadSource(source string) bool {
	parsed, err := url.Parse(source)
	if err != nil {
		return false
	}

	return parsed.Scheme == "" || parsed.Scheme == "file"
}

func probeLocalObject(source string) error {
	parsed, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parse local artifact source %q: %w", source, err)
	}

	path := source

	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return fmt.Errorf("file artifact source must not include host %q", parsed.Host)
		}

		path = parsed.Path
	}

	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("local artifact source must use an absolute path: %q", source)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open local artifact source %q: %w", path, err)
	}

	return file.Close()
}
