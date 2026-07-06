// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

type downloadSourceKind int

const (
	downloadSourceLocal downloadSourceKind = iota
	downloadSourceHTTP
	downloadSourceOCI
)

var downloadSourceHTTPClient = &http.Client{
	Timeout: 10 * time.Minute,
}

type downloadSource struct {
	raw       string
	kind      downloadSourceKind
	localPath string
}

func (s downloadSource) String() string {
	return s.raw
}

func parseDownloadSource(source string) (downloadSource, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return downloadSource{}, fmt.Errorf("parse artifact source %q: %w", source, err)
	}

	switch parsed.Scheme {
	case "":
		return parseLocalDownloadSource(source, parsed)
	case "file":
		return parseLocalDownloadSource(source, parsed)
	case "http", "https":
		return downloadSource{raw: source, kind: downloadSourceHTTP}, nil
	case "oci":
		return downloadSource{raw: source, kind: downloadSourceOCI}, nil
	default:
		return downloadSource{}, fmt.Errorf("unsupported artifact source scheme %q", parsed.Scheme)
	}
}

func parseLocalDownloadSource(source string, parsed *url.URL) (downloadSource, error) {
	path := source

	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return downloadSource{}, fmt.Errorf("file artifact source must not include host %q", parsed.Host)
		}

		path = parsed.Path
	}

	if path == "" || !filepath.IsAbs(path) {
		return downloadSource{}, fmt.Errorf("local artifact source must use an absolute path: %q", source)
	}

	return downloadSource{raw: source, kind: downloadSourceLocal, localPath: path}, nil
}

func (s downloadSource) open(ctx context.Context) (io.ReadCloser, error) {
	switch s.kind {
	case downloadSourceLocal:
		file, err := os.Open(s.localPath)
		if err != nil {
			return nil, fmt.Errorf("open local artifact source %q: %w", s.localPath, err)
		}

		return file, nil
	case downloadSourceHTTP:
		return openHTTPDownloadSource(ctx, s.raw)
	case downloadSourceOCI:
		return ociartifact.Open(ctx, s.raw)
	default:
		return nil, fmt.Errorf("unsupported artifact source kind %d", s.kind)
	}
}

func openHTTPDownloadSource(ctx context.Context, source string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	resp, err := downloadSourceHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck // body close
		return nil, fmt.Errorf("download %q failed with status code %d", source, resp.StatusCode)
	}

	return resp.Body, nil
}

func (s downloadSource) downloadToLocalFile(ctx context.Context, filename string, perm os.FileMode) error {
	body, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	return utilio.InstallFile(filename, body, perm)
}

func (s downloadSource) downloadWithSHA256Verification(ctx context.Context, expectedHash, filename string, perm os.FileMode) error {
	body, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	hasher := sha256.New()
	teeReader := io.TeeReader(body, hasher)

	if err := utilio.InstallFile(filename, teeReader, perm); err != nil {
		return err
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		_ = os.Remove(filename) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("SHA256 mismatch for %q: expected %s, got %s", s.raw, expectedHash, actualHash)
	}

	return nil
}

func readExpectedSHA256(ctx context.Context, checksumSource downloadSource) (string, error) {
	body, err := checksumSource.open(ctx)
	if err != nil {
		return "", err
	}
	defer body.Close() //nolint:errcheck // body close

	raw, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum body: %w", err)
	}

	hashStr := strings.TrimSpace(string(raw))
	if fields := strings.Fields(hashStr); len(fields) >= 1 {
		hashStr = fields[0]
	}

	if len(hashStr) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA256 hash length %d in checksum file", len(hashStr))
	}

	if _, err := hex.DecodeString(hashStr); err != nil {
		return "", fmt.Errorf("invalid hex in checksum file: %w", err)
	}

	return hashStr, nil
}

func (s downloadSource) decompressTarGz(ctx context.Context) utilio.TarFileSeq {
	return func(yield func(*utilio.TarFile, error) bool) {
		body, err := s.open(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer body.Close() //nolint:errcheck // body close

		for tarFile, err := range utilio.DecompressTarGz(body) {
			if !yield(tarFile, err) {
				return
			}
		}
	}
}

func (s downloadSource) probe(ctx context.Context) error {
	switch s.kind {
	case downloadSourceLocal, downloadSourceOCI:
		body, err := s.open(ctx)
		if err != nil {
			return err
		}

		return body.Close()
	case downloadSourceHTTP:
		return utilio.ProbeRemoteHTTPObject(ctx, s.raw)
	default:
		return fmt.Errorf("unsupported artifact source kind %d", s.kind)
	}
}
