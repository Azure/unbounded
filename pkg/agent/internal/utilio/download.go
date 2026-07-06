// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const remoteHTTPProbeTimeout = 10 * time.Second

var remoteHTTPClient = &http.Client{
	Timeout: 10 * time.Minute, // FIXME: proper configuration
}

var remoteHTTPProbeClient = &http.Client{
	Transport: newRemoteHTTPProbeTransport(),
	Timeout:   remoteHTTPProbeTimeout,
}

func newRemoteHTTPProbeTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}

	return transport.Clone()
}

func downloadFromRemote(ctx context.Context, source string) (io.ReadCloser, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse download source %q: %w", source, err)
	}

	switch parsed.Scheme {
	case "", "file":
		return openLocalSource(parsed, source)
	case "http", "https":
		return downloadHTTP(ctx, source)
	default:
		return nil, fmt.Errorf("unsupported download source scheme %q", parsed.Scheme)
	}
}

func downloadHTTP(ctx context.Context, source string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	resp, err := remoteHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck // body close
		return nil, fmt.Errorf("download %q failed with status code %d", source, resp.StatusCode)
	}

	return resp.Body, nil
}

func openLocalSource(parsed *url.URL, source string) (io.ReadCloser, error) {
	path := source

	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return nil, fmt.Errorf("file download source must not include host %q", parsed.Host)
		}

		path = parsed.Path
	}

	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("file download source must use an absolute path: %q", source)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file download source %q: %w", path, err)
	}

	return file, nil
}

// ProbeRemoteHTTPObject checks that an HTTP artifact object is reachable
// without downloading the full object. It first tries HEAD, then falls back to a
// ranged GET for servers that do not support or incorrectly reject HEAD.
func ProbeRemoteHTTPObject(ctx context.Context, source string) error {
	parsed, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parse download source %q: %w", source, err)
	}

	switch parsed.Scheme {
	case "http", "https":
		if err := probeRemoteHTTPObject(ctx, http.MethodHead, source); err == nil {
			return nil
		}

		return probeRemoteHTTPObject(ctx, http.MethodGet, source)
	default:
		return fmt.Errorf("unsupported HTTP artifact source scheme %q", parsed.Scheme)
	}
}

func probeRemoteHTTPObject(ctx context.Context, method, source string) error {
	req, err := http.NewRequestWithContext(ctx, method, source, http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	if method == http.MethodGet {
		// Keep the GET fallback non-mutating and lightweight. Range asks the
		// server for the first byte only, which is enough to prove that the
		// object is reachable when HEAD is unsupported. Go's HTTP transport does
		// not add transparent gzip negotiation to ranged requests, so no explicit
		// Accept-Encoding override is needed here.
		req.Header.Set("Range", "bytes=0-0")
	}

	resp, err := remoteHTTPProbeClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body close

	if isReachableHTTPStatus(resp.StatusCode) {
		return nil
	}

	return fmt.Errorf("remote object returned HTTP status %d", resp.StatusCode)
}

func isReachableHTTPStatus(status int) bool {
	return status == http.StatusOK || status == http.StatusPartialContent
}

type TarFile struct {
	Name string
	Size int64
	Body io.Reader
}

type TarFileSeq = iter.Seq2[*TarFile, error]

// DecompressTarGzFromRemote returns an iterator that yields the files contained in a .tar.gz file located at the given URL.
func DecompressTarGzFromRemote(ctx context.Context, url string) TarFileSeq {
	return func(yield func(*TarFile, error) bool) {
		body, err := downloadFromRemote(ctx, url)
		if err != nil {
			yield(nil, err)
			return
		}
		defer body.Close() //nolint:errcheck // body close

		for tarFile, err := range DecompressTarGz(body) {
			if !yield(tarFile, err) {
				return
			}
		}
	}
}

// DecompressTarGz returns an iterator that yields the files contained in a gzip-compressed tar stream.
func DecompressTarGz(body io.Reader) TarFileSeq {
	return func(yield func(*TarFile, error) bool) {
		gzipStream, err := gzip.NewReader(body)
		if err != nil {
			yield(nil, err)
			return
		}
		defer gzipStream.Close() //nolint:errcheck // gzip reader close

		tarReader := tar.NewReader(gzipStream)

		for {
			header, err := tarReader.Next()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				yield(nil, err)
				return
			}

			if header.Typeflag != tar.TypeReg {
				continue
			}

			cleanedName, err := cleanedTarEntryName(header.Name)
			if err != nil {
				yield(nil, fmt.Errorf("invalid tar entry %q: %w", header.Name, err))
				return
			}

			if !yield(&TarFile{Name: cleanedName, Size: header.Size, Body: tarReader}, nil) {
				return
			}
		}
	}
}

// cleanedTarEntryName validates and cleans a tar entry name to prevent path traversal attacks.
func cleanedTarEntryName(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}
	// Tar paths should be forward-slash. Reject backslashes to avoid odd edge cases.
	if strings.Contains(filename, `\`) || strings.ContainsRune(filename, '\x00') {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}

	cleaned := filepath.Clean(filepath.FromSlash(filename))
	if filepath.IsAbs(cleaned) ||
		cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid tar entry name: %q", filename)
	}

	return cleaned, nil
}

// DownloadToLocalFile downloads content from giving URL to local file and sets the specified permissions.
// It limits the size of the content to 1 GiB and returns an error if the limit is exceeded.
// It ensures that the target directory exists and handles the file writing atomically.
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func DownloadToLocalFile(ctx context.Context, url, filename string, perm os.FileMode) error {
	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	return InstallFile(filename, body, perm)
}

// DownloadWithSHA256Verification downloads content from the given URL and verifies it against the SHA256
// checksum fetched from checksumURL. The checksum file is expected to contain a hex-encoded SHA256 hash
// (optionally followed by whitespace and a filename, which is ignored).
//
// NOTE: we assume the filename is trusted and cleaned without path traversal characters.
func DownloadWithSHA256Verification(ctx context.Context, url, checksumURL, filename string, perm os.FileMode) error {
	expectedHash, err := fetchSHA256(ctx, checksumURL)
	if err != nil {
		return fmt.Errorf("fetch checksum from %q: %w", checksumURL, err)
	}

	body, err := downloadFromRemote(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	hasher := sha256.New()
	teeReader := io.TeeReader(body, hasher)

	if err := InstallFile(filename, teeReader, perm); err != nil {
		return err
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		// Remove the file that failed verification.
		_ = os.Remove(filename) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("SHA256 mismatch for %q: expected %s, got %s", url, expectedHash, actualHash)
	}

	return nil
}

// fetchSHA256 downloads and parses a SHA256 checksum file. The file is expected to contain a hex-encoded
// hash, optionally followed by whitespace and a filename (standard sha256sum output format).
func fetchSHA256(ctx context.Context, checksumURL string) (string, error) {
	body, err := downloadFromRemote(ctx, checksumURL)
	if err != nil {
		return "", err
	}
	defer body.Close() //nolint:errcheck // body close

	// Checksum files are small; limit to 1 KiB to prevent abuse.
	raw, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum body: %w", err)
	}

	// Parse: the file may be just the hex hash, or "hash  filename\n" (sha256sum format).
	hashStr := strings.TrimSpace(string(raw))
	if fields := strings.Fields(hashStr); len(fields) >= 1 {
		hashStr = fields[0]
	}

	if len(hashStr) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA256 hash length %d in checksum file", len(hashStr))
	}

	// Validate that the string is valid hex.
	if _, err := hex.DecodeString(hashStr); err != nil {
		return "", fmt.Errorf("invalid hex in checksum file: %w", err)
	}

	return hashStr, nil
}
