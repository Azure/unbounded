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
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const (
	defaultMaxArchiveBytes = 256 << 20 // 256 MiB
	defaultMaxBinaryBytes  = 256 << 20 // 256 MiB
)

// Layout describes caller-owned blue-green agent binary paths.
type Layout struct {
	// BinaryPath is an optional compatibility path included in collision validation.
	BinaryPath   string
	BluePath     string
	GreenPath    string
	CurrentPath  string
	LastGoodPath string
}

// InstallOptions configures a bounded agent release archive install.
type InstallOptions struct {
	DownloadURL       string
	ExpectedSHA256    string
	ExpectedMember    string
	Mode              os.FileMode
	MaxArchiveBytes   int64
	MaxExtractedBytes int64
	HTTPClient        *http.Client
	ExactMember       bool
}

// SwitchResult describes a completed blue-green binary switch.
type SwitchResult struct {
	PreviousPath string
	CurrentPath  string
}

type normalizedInstallOptions struct {
	options        InstallOptions
	parsedURL      *url.URL
	expectedDigest [sha256.Size]byte
	verifyDigest   bool
}

func normalizeInstallOptions(opts InstallOptions) (normalizedInstallOptions, error) {
	parsedURL, err := validateDownloadURL(opts.DownloadURL)
	if err != nil {
		return normalizedInstallOptions{}, err
	}

	var expectedDigest [sha256.Size]byte

	verifyDigest := strings.TrimSpace(opts.ExpectedSHA256) != ""
	if verifyDigest {
		expectedDigest, err = parseSHA256(opts.ExpectedSHA256)
		if err != nil {
			return normalizedInstallOptions{}, err
		}
	}

	opts.ExpectedMember = strings.TrimSpace(opts.ExpectedMember)
	if opts.ExpectedMember == "" || opts.ExpectedMember == "." || opts.ExpectedMember == ".." ||
		filepath.Base(opts.ExpectedMember) != opts.ExpectedMember {
		return normalizedInstallOptions{}, fmt.Errorf("expected archive member must be an exact base name without a path prefix")
	}

	if opts.Mode != 0 && opts.Mode.Perm() != opts.Mode {
		return normalizedInstallOptions{}, fmt.Errorf("agent binary mode must contain permission bits only")
	}

	if opts.MaxArchiveBytes < 0 || opts.MaxExtractedBytes < 0 {
		return normalizedInstallOptions{}, fmt.Errorf("agent archive size limits must not be negative")
	}

	if opts.MaxArchiveBytes > math.MaxInt64-1 {
		return normalizedInstallOptions{}, fmt.Errorf("maximum archive size is too large")
	}

	if opts.MaxExtractedBytes > math.MaxInt64/2 {
		return normalizedInstallOptions{}, fmt.Errorf("maximum extracted size is too large")
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

	opts.HTTPClient = boundedHTTPClient(opts.HTTPClient)

	return normalizedInstallOptions{
		options:        opts,
		parsedURL:      parsedURL,
		expectedDigest: expectedDigest,
		verifyDigest:   verifyDigest,
	}, nil
}

func validateLayout(paths Layout) error {
	values := []string{
		paths.BluePath,
		paths.GreenPath,
		paths.CurrentPath,
		paths.LastGoodPath,
	}
	if paths.BinaryPath != "" {
		values = append(values, paths.BinaryPath)
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

func installFromTarGz(ctx context.Context, targetPath string, opts InstallOptions) error {
	normalized, err := normalizeInstallOptions(opts)
	if err != nil {
		return err
	}

	opts = normalized.options

	archivePath, err := downloadArchive(
		ctx,
		opts.HTTPClient,
		normalized.parsedURL,
		normalized.expectedDigest,
		normalized.verifyDigest,
		opts.MaxArchiveBytes,
	)
	if err != nil {
		return err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup

	if err := extractOnlyArchiveMember(archivePath, targetPath, opts); err != nil {
		return err
	}

	return Verify(ctx, targetPath)
}

// InstallAndSwitchFromTarGz downloads a bounded HTTP or HTTPS release
// archive, installs the configured member into the inactive slot, and atomically
// updates the last-good and current links. When ExactMember is set, the archive
// must contain only the exact configured base name.
func InstallAndSwitchFromTarGz(
	ctx context.Context,
	log *slog.Logger,
	paths Layout,
	opts InstallOptions,
) (SwitchResult, error) {
	if err := validateLayout(paths); err != nil {
		return SwitchResult{}, err
	}

	normalized, err := normalizeInstallOptions(opts)
	if err != nil {
		return SwitchResult{}, err
	}

	opts = normalized.options
	parsedURL := normalized.parsedURL
	expectedDigest := normalized.expectedDigest
	verifyDigest := normalized.verifyDigest

	previousPath, err := executablePath(paths.CurrentPath)
	if err != nil {
		return SwitchResult{}, fmt.Errorf("resolve current agent binary: %w", err)
	}

	currentIsBlue, err := pathResolvesTo(paths.BluePath, previousPath)
	if err != nil {
		return SwitchResult{}, fmt.Errorf("resolve blue agent binary: %w", err)
	}

	targetPath := paths.BluePath
	if currentIsBlue {
		targetPath = paths.GreenPath
	}

	archivePath, err := downloadArchive(ctx, opts.HTTPClient, parsedURL, expectedDigest, verifyDigest, opts.MaxArchiveBytes)
	if err != nil {
		return SwitchResult{}, err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup

	lastGoodTarget, err := filepath.EvalSymlinks(paths.LastGoodPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return SwitchResult{}, fmt.Errorf("resolve last-good agent binary: %w", err)
	}

	canonicalTargetDir, err := filepath.EvalSymlinks(filepath.Dir(targetPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return SwitchResult{}, fmt.Errorf("resolve inactive agent binary directory: %w", err)
	}

	// Protect last-good before replacing the inactive slot only when that slot
	// contains last-good. Otherwise, preserve the existing rollback target until
	// the candidate has been staged and verified. A missing target directory
	// cannot contain a last-good binary.
	lastGoodProtected := err == nil && lastGoodTarget == filepath.Join(canonicalTargetDir, filepath.Base(targetPath))
	if lastGoodProtected {
		if err := utilio.UpdateSymlink(paths.LastGoodPath, previousPath); err != nil {
			return SwitchResult{}, fmt.Errorf("protect current agent as last-good: %w", err)
		}
	}

	if err := extractOnlyArchiveMember(archivePath, targetPath, opts); err != nil {
		return SwitchResult{}, err
	}

	if err := Verify(ctx, targetPath); err != nil {
		return SwitchResult{}, err
	}

	if !lastGoodProtected {
		if err := utilio.UpdateSymlink(paths.LastGoodPath, previousPath); err != nil {
			return SwitchResult{}, fmt.Errorf("update last-good agent symlink: %w", err)
		}
	}

	if err := utilio.UpdateSymlink(paths.CurrentPath, targetPath); err != nil {
		return SwitchResult{}, fmt.Errorf("update current agent symlink: %w", err)
	}

	log.Info("staged upgraded agent binary",
		"url", redactedURL(parsedURL),
		"previous", previousPath,
		"current", targetPath,
	)

	return SwitchResult{PreviousPath: previousPath, CurrentPath: targetPath}, nil
}

// RedactedURL removes query and fragment data that may contain credentials.
func redactedURL(parsedURL *url.URL) string {
	if parsedURL == nil {
		return ""
	}

	redacted := *parsedURL
	redacted.RawQuery = ""
	redacted.Fragment = ""

	return redacted.String()
}

func validateDownloadURL(rawURL string) (*url.URL, error) {
	parsedURL, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid download URL")
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported agent download URL scheme %q", parsedURL.Scheme)
	}

	if parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" {
		return nil, fmt.Errorf("download URL must include a host, omit user information, and omit fragments")
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

func boundedHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Minute}
	}

	client := *base
	originalCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" ||
			req.URL.User != nil || req.URL.Fragment != "" {
			return fmt.Errorf("redirect URL must use HTTP or HTTPS, include a host, omit user information, and omit fragments")
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

func downloadArchive(
	ctx context.Context,
	client *http.Client,
	parsedURL *url.URL,
	expected [sha256.Size]byte,
	verifyDigest bool,
	maxBytes int64,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create agent archive request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("download agent archive from %s: %w", redactedURL(parsedURL), ctx.Err())
		}
		// Redirect and transport errors can contain credential-bearing URLs.
		return "", fmt.Errorf("download agent archive from %s failed", redactedURL(parsedURL))
	}
	defer resp.Body.Close() //nolint:errcheck // response body cleanup

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download agent archive from %s: HTTP status %d", redactedURL(parsedURL), resp.StatusCode)
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

	if verifyDigest && !equalDigest(hasher.Sum(nil), expected[:]) {
		return "", fmt.Errorf("agent archive SHA-256 does not match expected digest")
	}

	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary agent archive: %w", err)
	}

	ok = true

	return path, nil
}

func extractOnlyArchiveMember(archivePath, targetPath string, opts InstallOptions) (err error) {
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
	stagedPath := ""

	defer func() {
		if stagedPath != "" {
			os.Remove(stagedPath) //nolint:errcheck // best effort staged file cleanup
		}
	}()

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

		memberName := filepath.Clean(header.Name)
		if !safeArchiveName(header.Name, opts.ExactMember) {
			return fmt.Errorf("agent archive contains unsafe member name %q", header.Name)
		}

		if opts.ExactMember && header.Name != opts.ExpectedMember {
			return fmt.Errorf("agent archive contains unexpected member %q", header.Name)
		}

		if !opts.ExactMember && filepath.Base(memberName) != opts.ExpectedMember {
			continue
		}

		if found {
			return fmt.Errorf("agent archive contains duplicate member %q", opts.ExpectedMember)
		}

		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > opts.MaxExtractedBytes {
			return fmt.Errorf("agent archive member %q is not a valid bounded regular file", opts.ExpectedMember)
		}

		if mkdirErr := os.MkdirAll(filepath.Dir(targetPath), 0o750); mkdirErr != nil {
			return fmt.Errorf("create agent binary directory: %w", mkdirErr)
		}

		staged, createErr := os.CreateTemp(filepath.Dir(targetPath), ".agent-upgrade-*")
		if createErr != nil {
			return fmt.Errorf("create staged agent binary: %w", createErr)
		}

		stagedPath = staged.Name()
		if closeErr := staged.Close(); closeErr != nil {
			return fmt.Errorf("close staged agent binary: %w", closeErr)
		}

		if err := utilio.InstallFileWithLimitedSize(stagedPath, tarReader, opts.Mode, opts.MaxExtractedBytes); err != nil {
			return fmt.Errorf("stage upgraded agent binary: %w", err)
		}

		found = true
	}

	if _, err := io.Copy(io.Discard, decompressed); err != nil {
		return fmt.Errorf("finish reading agent archive: %w", err)
	}

	if decompressed.count > 2*opts.MaxExtractedBytes {
		return fmt.Errorf("decompressed agent archive exceeds %d-byte limit", 2*opts.MaxExtractedBytes)
	}

	if !found {
		return fmt.Errorf("agent archive does not contain expected member %q", opts.ExpectedMember)
	}

	if err := os.Rename(stagedPath, targetPath); err != nil {
		return fmt.Errorf("install upgraded agent binary: %w", err)
	}

	stagedPath = ""

	return nil
}

func safeArchiveName(name string, exact bool) bool {
	cleaned := filepath.Clean(name)
	if name == "" || cleaned == "." || cleaned == ".." || filepath.IsAbs(name) ||
		strings.Contains(name, `\`) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return false
	}

	return !exact || cleaned == name
}

func pathResolvesTo(path, expected string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return resolved == expected, nil
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
