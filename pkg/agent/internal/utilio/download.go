// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
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
	if err := validateHTTPDownloadSource(source); err != nil {
		return nil, err
	}

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

func validateHTTPDownloadSource(source string) error {
	parsed, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parse download source %q: %w", source, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported download source scheme %q", parsed.Scheme)
	}

	return nil
}

// ProbeRemoteHTTPObject checks that an HTTP artifact object is reachable
// without downloading the full object. It first tries HEAD, then falls back to a
// ranged GET for servers that do not support or incorrectly reject HEAD.
func ProbeRemoteHTTPObject(ctx context.Context, source string) error {
	if err := validateHTTPDownloadSource(source); err != nil {
		return err
	}

	if err := probeRemoteHTTPObject(ctx, http.MethodHead, source); err == nil {
		return nil
	}

	return probeRemoteHTTPObject(ctx, http.MethodGet, source)
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

// DecompressTarGzFromRemote returns an iterator that yields the files contained in a .tar.gz file located at the given HTTP(S) URL.
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
