// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
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

// InstallOptions configures an agent release archive install.
type InstallOptions struct {
	DownloadURL string
	// ExpectedSHA256 may be empty only when the caller trusts both the archive
	// source and the transport path.
	ExpectedSHA256 string
	// ExpectedMember is the file to install from the archive root, with or
	// without a "./" prefix. Every other archive entry is ignored, including
	// entries elsewhere in the archive with the same name.
	ExpectedMember string
	Mode           os.FileMode
	HTTPClient     *http.Client
	// Check, when set, runs on the staged candidate after it passes Verify and
	// before any link is switched. An error refuses the candidate and leaves
	// the running agent selected.
	Check func(ctx context.Context, path string) error
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

	if opts.Mode == 0 {
		opts.Mode = daemonBinaryMode
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

		canonical, err := canonicalPathEntry(value)
		if err != nil {
			return fmt.Errorf("resolve agent binary path %q: %w", value, err)
		}

		if _, ok := seen[canonical]; ok {
			return fmt.Errorf("duplicate agent binary path %q", value)
		}

		seen[canonical] = struct{}{}
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
	)
	if err != nil {
		return err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup

	if err := extractArchiveMember(archivePath, targetPath, opts); err != nil {
		return err
	}

	return Verify(ctx, targetPath)
}

// InstallAndSwitchFromTarGz downloads an HTTP or HTTPS release archive,
// installs the configured member into the inactive slot, and atomically
// updates the last-good and current links.
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

	archivePath, err := downloadArchive(ctx, opts.HTTPClient, parsedURL, expectedDigest, verifyDigest)
	if err != nil {
		return SwitchResult{}, err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup

	lastGoodProtected, err := symlinkReferencesPath(paths.LastGoodPath, targetPath)
	if err != nil {
		return SwitchResult{}, fmt.Errorf("resolve last-good agent binary: %w", err)
	}

	// Protect last-good before replacing the inactive slot only when that slot
	// contains last-good. Otherwise, preserve the existing rollback target until
	// the candidate has been staged and verified.
	if lastGoodProtected {
		if err := utilio.UpdateSymlink(paths.LastGoodPath, previousPath); err != nil {
			return SwitchResult{}, fmt.Errorf("protect current agent as last-good: %w", err)
		}
	}

	if err := extractArchiveMember(archivePath, targetPath, opts); err != nil {
		return SwitchResult{}, err
	}

	if err := Verify(ctx, targetPath); err != nil {
		return SwitchResult{}, err
	}

	if opts.Check != nil {
		if err := opts.Check(ctx, targetPath); err != nil {
			return SwitchResult{}, fmt.Errorf("refuse agent binary %s: %w", targetPath, err)
		}
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
		if originalCheckRedirect != nil {
			if err := originalCheckRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}

		if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" ||
			req.URL.User != nil || req.URL.Fragment != "" {
			return fmt.Errorf("redirect URL must use HTTP or HTTPS, include a host, omit user information, and omit fragments")
		}

		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("HTTPS download cannot redirect to HTTP")
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

	if _, err := io.Copy(io.MultiWriter(temp, hasher), resp.Body); err != nil {
		return "", fmt.Errorf("read agent archive: %w", err)
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

// extractArchiveMember installs the archive's ExpectedMember at targetPath. The
// binary is staged beside targetPath and renamed into place only after the
// archive is accepted, so a rejected archive leaves targetPath unchanged.
func extractArchiveMember(archivePath, targetPath string, opts InstallOptions) (err error) {
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

	tarReader := tar.NewReader(gz)

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("agent archive does not contain %q at its root", opts.ExpectedMember)
		}

		if err != nil {
			return fmt.Errorf("read agent archive: %w", err)
		}

		if strings.TrimPrefix(header.Name, "./") != opts.ExpectedMember {
			continue
		}

		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("agent archive member %q is not a regular file", header.Name)
		}

		if header.Size == 0 {
			return fmt.Errorf("agent archive member %q is empty", header.Name)
		}

		stagedPath, err := stageArchiveMember(targetPath, tarReader, opts.Mode)
		if err != nil {
			return err
		}
		defer os.Remove(stagedPath) //nolint:errcheck // no-op once the binary is renamed into place

		// gzip verifies its checksum only at the end of the stream, and that
		// checksum is all that catches a corrupted download without a SHA-256.
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return fmt.Errorf("read agent archive: %w", err)
		}

		if err := os.Rename(stagedPath, targetPath); err != nil {
			return fmt.Errorf("install upgraded agent binary: %w", err)
		}

		return nil
	}
}

// stageArchiveMember writes body to a new file in targetPath's directory and
// returns its path. The caller renames or removes it.
func stageArchiveMember(targetPath string, body io.Reader, mode os.FileMode) (string, error) {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create agent binary directory: %w", err)
	}

	staged, err := os.CreateTemp(dir, ".agent-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("create staged agent binary: %w", err)
	}

	_, err = io.Copy(staged, body)
	if err == nil {
		err = staged.Chmod(mode)
	}

	if err == nil {
		err = staged.Sync()
	}

	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		os.Remove(staged.Name()) //nolint:errcheck // best effort staged file cleanup
		return "", fmt.Errorf("stage upgraded agent binary: %w", err)
	}

	return staged.Name(), nil
}

func canonicalPathEntry(path string) (string, error) {
	dir := filepath.Dir(path)
	missing := make([]string, 0)

	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}

			return filepath.Join(resolved, filepath.Base(path)), nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}

		missing = append(missing, filepath.Base(dir))
		dir = parent
	}
}

func symlinkReferencesPath(linkPath, targetPath string) (bool, error) {
	targetEntry, err := canonicalPathEntry(targetPath)
	if err != nil {
		return false, err
	}

	resolved, err := filepath.EvalSymlinks(linkPath)
	if err == nil {
		return resolved == targetEntry, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	info, lstatErr := os.Lstat(linkPath)
	if errors.Is(lstatErr, os.ErrNotExist) {
		return false, nil
	}

	if lstatErr != nil {
		return false, lstatErr
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return false, nil
	}

	intended, err := os.Readlink(linkPath)
	if err != nil {
		return false, err
	}

	if !filepath.IsAbs(intended) {
		intended = filepath.Join(filepath.Dir(linkPath), intended)
	}

	intendedEntry, err := canonicalPathEntry(filepath.Clean(intended))
	if err != nil {
		return false, err
	}

	return intendedEntry == targetEntry, nil
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
