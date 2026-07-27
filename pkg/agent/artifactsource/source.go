// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package artifactsource opens and probes agent artifact download sources.
package artifactsource

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

	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

type sourceKind int

const (
	sourceLocal sourceKind = iota
	sourceHTTP
	sourceOCI
)

const (
	httpDownloadMaxAttempts = 5
	httpDownloadRetryDelay  = 2 * time.Second
)

var httpClient = &http.Client{
	Timeout:       10 * time.Minute,
	CheckRedirect: utilio.CheckRedirectNoHTTPSDowngrade,
	Transport: &retry.Transport{
		Policy: newHTTPDownloadRetryPolicy,
	},
}

func newHTTPDownloadRetryPolicy() retry.Policy {
	return &retry.GenericPolicy{
		Retryable: retryHTTPDownloadFailure,
		Backoff:   httpDownloadBackoff,
		MinWait:   httpDownloadRetryDelay,
		MaxWait:   maxHTTPDownloadRetryDelay(),
		MaxRetry:  httpDownloadMaxAttempts - 1,
	}
}

func retryHTTPDownloadFailure(resp *http.Response, err error) (bool, error) {
	if ociutil.RetryableNetworkError(err) {
		return true, nil
	}

	if resp == nil {
		return false, nil
	}

	return retry.DefaultPredicate(resp, nil)
}

func httpDownloadBackoff(attempt int, _ *http.Response) time.Duration {
	delay := httpDownloadRetryDelay
	for range attempt {
		delay *= 2
	}

	return delay
}

func maxHTTPDownloadRetryDelay() time.Duration {
	delay := httpDownloadRetryDelay
	for range httpDownloadMaxAttempts - 2 {
		delay *= 2
	}

	return delay
}

// Source is a parsed, openable artifact source. It can reference an absolute
// local path, file:// URL, HTTP(S) URL, or OCI artifact blob using
// oci://...#title.
type Source struct {
	raw       string
	kind      sourceKind
	localPath string
}

// Parse validates and parses an openable artifact source string.
func Parse(source string) (Source, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return Source{}, fmt.Errorf("parse artifact source: %w", utilio.RedactHTTPError(err))
	}

	switch parsed.Scheme {
	case "":
		return parseLocal(source, parsed)
	case "file":
		return parseLocal(source, parsed)
	case "http", "https":
		return Source{raw: source, kind: sourceHTTP}, nil
	case "oci":
		if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
			return Source{}, fmt.Errorf("OCI artifact source must include registry and repository")
		}

		if parsed.User != nil || parsed.RawQuery != "" {
			return Source{}, fmt.Errorf("OCI artifact source must not include user info or query parameters")
		}

		if strings.TrimPrefix(parsed.Fragment, "/") == "" {
			return Source{}, fmt.Errorf("OCI artifact source must include a blob title fragment")
		}

		return Source{raw: source, kind: sourceOCI}, nil
	default:
		return Source{}, fmt.Errorf("unsupported artifact source scheme %q", parsed.Scheme)
	}
}

func parseLocal(source string, parsed *url.URL) (Source, error) {
	path := source

	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return Source{}, fmt.Errorf("file artifact source must not include host %q", parsed.Host)
		}

		unescapedPath, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return Source{}, fmt.Errorf("unescape file artifact source path %q: %w", parsed.Path, err)
		}

		path = filepath.Clean(unescapedPath)
	}

	if path == "" || !filepath.IsAbs(path) {
		return Source{}, fmt.Errorf("local artifact source must use an absolute path: %q", source)
	}

	return Source{raw: source, kind: sourceLocal, localPath: path}, nil
}

// String returns the original artifact source string.
func (s Source) String() string {
	return s.raw
}

// Open opens the artifact source for streaming.
func (s Source) Open(ctx context.Context) (io.ReadCloser, error) {
	switch s.kind {
	case sourceLocal:
		file, err := os.Open(s.localPath)
		if err != nil {
			return nil, fmt.Errorf("open local artifact source %q: %w", s.localPath, err)
		}

		return file, nil
	case sourceHTTP:
		return openHTTP(ctx, s.raw)
	case sourceOCI:
		return ociartifact.Open(ctx, s.raw)
	default:
		return nil, fmt.Errorf("unsupported artifact source kind %d", s.kind)
	}
}

func openHTTP(ctx context.Context, source string) (io.ReadCloser, error) {
	return openHTTPWithClient(ctx, httpClient, source)
}

func openHTTPWithClient(ctx context.Context, client *http.Client, source string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", utilio.RedactHTTPError(err))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", utilio.RedactHTTPError(err))
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck // body close
		return nil, fmt.Errorf("download %q failed with status code %d", utilio.RedactURLQuery(source), resp.StatusCode)
	}

	return resp.Body, nil
}

// ReadAll reads the complete artifact source.
func (s Source) ReadAll(ctx context.Context) ([]byte, error) {
	body, err := s.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read artifact source: %w", err)
	}

	return data, nil
}

// DownloadToLocalFile downloads the artifact source to filename and sets perm.
func (s Source) DownloadToLocalFile(ctx context.Context, filename string, perm os.FileMode) error {
	body, err := s.Open(ctx)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	return utilio.InstallFile(filename, body, perm)
}

// DownloadWithSHA256Verification downloads the artifact source to filename and
// verifies the downloaded content against expectedHash.
func (s Source) DownloadWithSHA256Verification(ctx context.Context, expectedHash, filename string, perm os.FileMode) error {
	body, err := s.Open(ctx)
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
		return fmt.Errorf("SHA256 mismatch for %q: expected %s, got %s", utilio.RedactURLQuery(s.raw), expectedHash, actualHash)
	}

	return nil
}

// ReadExpectedSHA256 reads a sha256sum-format checksum source and returns the
// expected hex-encoded SHA256 hash.
func ReadExpectedSHA256(ctx context.Context, checksumSource Source) (string, error) {
	body, err := checksumSource.Open(ctx)
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

// ExtractTar extracts a tar or gzip-compressed tar artifact into destDir.
func (s Source) ExtractTar(ctx context.Context, destDir string) error {
	body, err := s.Open(ctx)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // best effort close

	return utilio.ExtractTar(body, destDir)
}

// DecompressTarGz returns an iterator that yields files from a gzip-compressed
// tar artifact source.
func (s Source) DecompressTarGz(ctx context.Context) utilio.TarFileSeq {
	return func(yield func(*utilio.TarFile, error) bool) {
		body, err := s.Open(ctx)
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

// Probe checks that the artifact source is reachable without installing it.
func (s Source) Probe(ctx context.Context) error {
	switch s.kind {
	case sourceLocal, sourceOCI:
		body, err := s.Open(ctx)
		if err != nil {
			return err
		}

		return body.Close()
	case sourceHTTP:
		return utilio.ProbeRemoteHTTPObject(ctx, s.raw)
	default:
		return fmt.Errorf("unsupported artifact source kind %d", s.kind)
	}
}
