// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const (
	defaultMaxArchiveBytes = 256 << 20
	defaultMaxBinaryBytes  = 256 << 20
)

// Layout describes caller-owned blue-green agent binary paths.
type Layout struct {
	BinaryPath   string
	BluePath     string
	GreenPath    string
	CurrentPath  string
	LastGoodPath string
}

// SecureInstallOptions configures a verified agent release archive install.
type SecureInstallOptions struct {
	DownloadURL       string
	ExpectedSHA256    string
	ExpectedMember    string
	Mode              os.FileMode
	MaxArchiveBytes   int64
	MaxExtractedBytes int64
	HTTPClient        *http.Client
}

// SwitchResult describes a completed blue-green binary switch.
type SwitchResult struct {
	PreviousPath string
	CurrentPath  string
}

type normalizedSecureInstallOptions struct {
	options        SecureInstallOptions
	parsedURL      *url.URL
	expectedDigest [sha256.Size]byte
}

// ValidateSecureInstallOptions validates caller-provided secure install inputs.
func ValidateSecureInstallOptions(opts SecureInstallOptions) error {
	_, err := normalizeSecureInstallOptions(opts)

	return err
}

func normalizeSecureInstallOptions(opts SecureInstallOptions) (normalizedSecureInstallOptions, error) {
	parsedURL, err := validateSecureDownloadURL(opts.DownloadURL)
	if err != nil {
		return normalizedSecureInstallOptions{}, err
	}

	expectedDigest, err := parseSHA256(opts.ExpectedSHA256)
	if err != nil {
		return normalizedSecureInstallOptions{}, err
	}

	opts.ExpectedMember = strings.TrimSpace(opts.ExpectedMember)
	if opts.ExpectedMember == "" || filepath.Base(opts.ExpectedMember) != opts.ExpectedMember {
		return normalizedSecureInstallOptions{}, fmt.Errorf("expected archive member must be an exact base name without a path prefix")
	}

	if opts.Mode != 0 && opts.Mode.Perm() != opts.Mode {
		return normalizedSecureInstallOptions{}, fmt.Errorf("agent binary mode must contain permission bits only")
	}

	if opts.MaxArchiveBytes < 0 || opts.MaxExtractedBytes < 0 {
		return normalizedSecureInstallOptions{}, fmt.Errorf("agent archive size limits must not be negative")
	}

	if opts.MaxArchiveBytes > math.MaxInt64-1 {
		return normalizedSecureInstallOptions{}, fmt.Errorf("maximum archive size is too large")
	}

	if opts.MaxExtractedBytes > math.MaxInt64/2 {
		return normalizedSecureInstallOptions{}, fmt.Errorf("maximum extracted size is too large")
	}

	if opts.Mode == 0 {
		opts.Mode = daemonBinaryMode
	}

	if opts.MaxArchiveBytes == 0 {
		opts.MaxArchiveBytes = defaultMaxArchiveBytes
	}

	if opts.MaxExtractedBytes == 0 {
		opts.MaxExtractedBytes = defaultMaxBinaryBytes
	}

	opts.HTTPClient = secureHTTPClient(opts.HTTPClient)

	return normalizedSecureInstallOptions{
		options:        opts,
		parsedURL:      parsedURL,
		expectedDigest: expectedDigest,
	}, nil
}

// ValidateLayout verifies that all binary paths are clean, absolute, and distinct.
func ValidateLayout(paths Layout) error {
	values := []string{
		paths.BinaryPath,
		paths.BluePath,
		paths.GreenPath,
		paths.CurrentPath,
		paths.LastGoodPath,
	}

	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("invalid agent binary path %q", value)
		}

		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate agent binary path %q", value)
		}

		seen[value] = struct{}{}
	}

	return nil
}

// SecureInstallAndSwitch downloads and verifies an HTTPS release archive,
// installs its only member into the inactive slot, and atomically updates the
// last-good and current links. The archive member must exactly equal the
// configured base name; path prefixes such as "./" are intentionally rejected.
func SecureInstallAndSwitch(
	ctx context.Context,
	log *slog.Logger,
	paths Layout,
	opts SecureInstallOptions,
) (SwitchResult, error) {
	if log == nil {
		return SwitchResult{}, fmt.Errorf("logger is nil")
	}

	if err := ValidateLayout(paths); err != nil {
		return SwitchResult{}, err
	}

	normalized, err := normalizeSecureInstallOptions(opts)
	if err != nil {
		return SwitchResult{}, err
	}

	opts = normalized.options
	parsedURL := normalized.parsedURL
	expectedDigest := normalized.expectedDigest

	previousPath, err := executablePath(paths.CurrentPath)
	if err != nil {
		return SwitchResult{}, fmt.Errorf("resolve current agent binary: %w", err)
	}

	targetPath := paths.BluePath
	if previousPath == paths.BluePath {
		targetPath = paths.GreenPath
	}

	archivePath, err := downloadVerifiedArchive(ctx, opts.HTTPClient, parsedURL, expectedDigest, opts.MaxArchiveBytes)
	if err != nil {
		return SwitchResult{}, err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup

	// The inactive slot may still be last-good. Protect the verified running
	// binary before replacing that slot.
	if err := utilio.UpdateSymlink(paths.LastGoodPath, previousPath); err != nil {
		return SwitchResult{}, fmt.Errorf("protect current agent as last-good: %w", err)
	}

	if err := extractOnlyArchiveMember(archivePath, targetPath, opts); err != nil {
		return SwitchResult{}, err
	}

	if err := verifyQuiet(ctx, targetPath); err != nil {
		return SwitchResult{}, err
	}

	if err := utilio.UpdateSymlink(paths.CurrentPath, targetPath); err != nil {
		return SwitchResult{}, fmt.Errorf("update current agent symlink: %w", err)
	}

	log.Info("staged upgraded agent binary",
		"url", RedactedURL(parsedURL),
		"previous", previousPath,
		"current", targetPath,
	)

	return SwitchResult{PreviousPath: previousPath, CurrentPath: targetPath}, nil
}

// RedactedURL removes query and fragment data that may contain credentials.
func RedactedURL(parsedURL *url.URL) string {
	redacted := *parsedURL
	redacted.RawQuery = ""
	redacted.Fragment = ""

	return redacted.String()
}

func validateSecureDownloadURL(rawURL string) (*url.URL, error) {
	parsedURL, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid download URL")
	}

	if parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" {
		return nil, fmt.Errorf("download URL must use HTTPS, include a host, omit user information, and omit fragments")
	}

	return parsedURL, nil
}

func parseSHA256(value string) ([sha256.Size]byte, error) {
	var expected [sha256.Size]byte

	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "sha256:")

	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return expected, fmt.Errorf("expected SHA-256 must be exactly 64 hexadecimal characters")
	}

	copy(expected[:], decoded)

	return expected, nil
}

func secureHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}

	client := *base
	originalCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("redirect to non-HTTPS URL is not allowed")
		}

		if originalCheckRedirect != nil {
			return originalCheckRedirect(req, via)
		}

		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}

		return nil
	}

	return &client
}

func downloadVerifiedArchive(
	ctx context.Context,
	client *http.Client,
	parsedURL *url.URL,
	expected [sha256.Size]byte,
	maxBytes int64,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create agent archive request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("download agent archive from %s: %w", RedactedURL(parsedURL), ctx.Err())
		}
		// Redirect and transport errors can contain credential-bearing URLs.
		return "", fmt.Errorf("download agent archive from %s failed", RedactedURL(parsedURL))
	}
	defer resp.Body.Close() //nolint:errcheck // response body cleanup

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download agent archive from %s: HTTP status %d", RedactedURL(parsedURL), resp.StatusCode)
	}

	if resp.ContentLength > maxBytes {
		return "", fmt.Errorf("agent archive exceeds %d-byte limit", maxBytes)
	}

	temp, err := os.CreateTemp("", "agent-upgrade-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create temporary agent archive: %w", err)
	}

	path := temp.Name()
	ok := false

	defer func() {
		temp.Close() //nolint:errcheck // best effort cleanup after an earlier failure

		if !ok {
			os.Remove(path) //nolint:errcheck // best effort temporary file cleanup
		}
	}()

	hasher := sha256.New()

	n, err := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read agent archive: %w", err)
	}

	if n > maxBytes {
		return "", fmt.Errorf("agent archive exceeds %d-byte limit", maxBytes)
	}

	if !equalDigest(hasher.Sum(nil), expected[:]) {
		return "", fmt.Errorf("agent archive SHA-256 does not match expected digest")
	}

	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary agent archive: %w", err)
	}

	ok = true

	return path, nil
}

func extractOnlyArchiveMember(archivePath, targetPath string, opts SecureInstallOptions) (err error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open agent archive: %w", err)
	}

	defer func() {
		if closeErr := archive.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("decompress agent archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // extraction reports read errors

	found := false
	decompressed := &countingReader{reader: io.LimitReader(gz, 2*opts.MaxExtractedBytes+1)}

	tarReader := tar.NewReader(decompressed)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}

		if nextErr != nil {
			return fmt.Errorf("read agent archive: %w", nextErr)
		}

		if !safeArchiveName(header.Name) {
			return fmt.Errorf("agent archive contains unsafe member name %q", header.Name)
		}

		if header.Name != opts.ExpectedMember {
			return fmt.Errorf("agent archive contains unexpected member %q", header.Name)
		}

		if found {
			return fmt.Errorf("agent archive contains duplicate member %q", opts.ExpectedMember)
		}

		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > opts.MaxExtractedBytes {
			return fmt.Errorf("agent archive member %q is not a valid bounded regular file", opts.ExpectedMember)
		}

		if err := utilio.InstallFileWithLimitedSize(targetPath, tarReader, opts.Mode, opts.MaxExtractedBytes); err != nil {
			return fmt.Errorf("install upgraded agent binary: %w", err)
		}

		found = true
	}

	if decompressed.count > 2*opts.MaxExtractedBytes {
		return fmt.Errorf("decompressed agent archive exceeds %d-byte limit", 2*opts.MaxExtractedBytes)
	}

	if !found {
		return fmt.Errorf("agent archive does not contain expected member %q", opts.ExpectedMember)
	}

	return nil
}

func safeArchiveName(name string) bool {
	return name != "" &&
		!filepath.IsAbs(name) &&
		filepath.Clean(name) == name &&
		!strings.Contains(name, `\`) &&
		!strings.HasPrefix(name, ".."+string(filepath.Separator))
}

func executablePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not a regular executable file", path)
	}

	return resolved, nil
}

func verifyQuiet(ctx context.Context, path string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(verifyCtx, path, "version")
	cmd.Stdout = io.Discard

	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify upgraded agent binary: %w", err)
	}

	return nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.count += int64(n)

	return n, err
}

func equalDigest(actual, expected []byte) bool {
	if len(actual) != len(expected) {
		return false
	}

	var different byte
	for i := range actual {
		different |= actual[i] ^ expected[i]
	}

	return different == 0
}
