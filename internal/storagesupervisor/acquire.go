// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

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
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxArchiveBytes caps the size of a downloaded tarball to guard against
// runaway or malicious responses (1 GiB).
const maxArchiveBytes = 1 << 30

// httpClient is used for all release/url downloads.
var httpClient = &http.Client{Timeout: 10 * time.Minute}

// acquireAndExtract resolves the configured source into a release-layout tree
// under stagingDir (so stagingDir/bin/unbounded-storage exists). For release
// and url sources the tarball and its companion .sha256 are downloaded and
// verified before extraction; a file source is extracted as-is.
func acquireAndExtract(ctx context.Context, cfg Config, stagingDir string) error {
	switch cfg.SourceMode {
	case SourceFile:
		slog.Info("installing from local tarball", "path", cfg.Source, "arch", cfg.Arch, "version", cfg.Version)

		return extractTarGz(cfg.Source, stagingDir)
	case SourceURL:
		return acquireRemote(ctx, cfg, cfg.Source, stagingDir)
	case SourceRelease:
		url := releaseTarballURL(cfg)
		slog.Info("installing from release", "repo", cfg.Repo, "arch", cfg.Arch, "version", cfg.Version)

		return acquireRemote(ctx, cfg, url, stagingDir)
	default:
		return fmt.Errorf("unknown source mode %d", cfg.SourceMode)
	}
}

// acquireRemote downloads a tarball and its sibling .sha256, verifies the
// checksum, and extracts the verified archive into stagingDir.
func acquireRemote(ctx context.Context, cfg Config, url, stagingDir string) error {
	slog.Info("downloading tarball", "url", url)

	archive, err := os.CreateTemp("", "unbounded-storage-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp archive: %w", err)
	}

	archivePath := archive.Name()

	defer os.Remove(archivePath) //nolint:errcheck // best-effort cleanup
	defer archive.Close()        //nolint:errcheck // closed again explicitly below

	if err := downloadTo(ctx, url, archive); err != nil {
		return err
	}

	if err := archive.Close(); err != nil {
		return fmt.Errorf("close temp archive: %w", err)
	}

	slog.Info("verifying checksum", "url", url+".sha256")

	expected, err := fetchSHA256(ctx, url+".sha256")
	if err != nil {
		return fmt.Errorf("fetch checksum: %w", err)
	}

	actual, err := sha256OfFile(archivePath)
	if err != nil {
		return err
	}

	if actual != expected {
		return fmt.Errorf("checksum verification failed for %q: expected %s, got %s", url, expected, actual)
	}

	slog.Info("checksum ok")

	return extractTarGz(archivePath, stagingDir)
}

// releaseTarballURL builds the GitHub release download URL for the configured
// repo, version, and architecture. "latest" uses the latest-release redirect so
// the URL keeps working across future releases.
func releaseTarballURL(cfg Config) string {
	var base string
	if cfg.Version == "latest" {
		base = fmt.Sprintf("https://github.com/%s/releases/latest/download", cfg.Repo)
	} else {
		base = fmt.Sprintf("https://github.com/%s/releases/download/%s", cfg.Repo, cfg.Version)
	}

	return base + "/" + tarballName(cfg.Arch)
}

// tarballName returns the release-layout archive file name for an architecture.
func tarballName(arch string) string {
	return fmt.Sprintf("unbounded-storage-linux-%s.tar.gz", arch)
}

// downloadTo streams the body of url into w, enforcing maxArchiveBytes.
func downloadTo(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %q: %w", url, err)
	}

	defer resp.Body.Close() //nolint:errcheck // body close

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %q failed with status %d", url, resp.StatusCode)
	}

	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxArchiveBytes)); err != nil {
		return fmt.Errorf("write download %q: %w", url, err)
	}

	return nil
}

// fetchSHA256 downloads and parses a sha256sum-format checksum file, returning
// the hex-encoded digest.
func fetchSHA256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %q: %w", url, err)
	}

	defer resp.Body.Close() //nolint:errcheck // body close

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %q failed with status %d", url, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum: %w", err)
	}

	return parseSHA256(string(raw))
}

// parseSHA256 extracts the hex digest from sha256sum output, which may be a bare
// hash or "hash  filename".
func parseSHA256(raw string) (string, error) {
	hashStr := strings.TrimSpace(raw)
	if fields := strings.Fields(hashStr); len(fields) >= 1 {
		hashStr = fields[0]
	}

	if len(hashStr) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA256 length %d in checksum file", len(hashStr))
	}

	if _, err := hex.DecodeString(hashStr); err != nil {
		return "", fmt.Errorf("invalid hex in checksum file: %w", err)
	}

	return hashStr, nil
}

// sha256OfFile computes the hex-encoded SHA256 of a file's contents.
func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}

	defer f.Close() //nolint:errcheck // read-only close

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz extracts a gzip-compressed tar archive into destDir, stripping
// the single top-level directory (equivalent to tar --strip-components=1) so
// bin/ and lib/ land directly under destDir. Entry names are validated to
// prevent path traversal.
func resolvePathAllowingNonExistent(path string) (string, error) {
	cleaned := filepath.Clean(path)
	cur := cleaned
	var tail []string

	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}

		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(cur)
			if parent == cur {
				return "", err
			}
			tail = append(tail, filepath.Base(cur))
			cur = parent
			continue
		}

		return "", err
	}
}

func isPathWithinBase(base, candidate string) (bool, error) {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false, fmt.Errorf("resolve base %q: %w", base, err)
	}

	resolvedCandidate, err := resolvePathAllowingNonExistent(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve candidate %q: %w", candidate, err)
	}

	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil {
		return false, err
	}

	rel = filepath.Clean(rel)
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)), nil
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %q: %w", archivePath, err)
	}

	defer f.Close() //nolint:errcheck // read-only close

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}

	defer gz.Close() //nolint:errcheck // reader close

	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		rel, ok := stripComponent(header.Name)
		if !ok {
			// Top-level entry itself (the stripped directory); skip it.
			continue
		}

		target, err := safeJoin(destDir, rel)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensurePathWithinBase(destDir, target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
		case tar.TypeReg:
			if err := ensurePathWithinBase(destDir, target); err != nil {
				return err
			}
			if err := writeFileFromTar(tr, target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := ensurePathWithinBase(destDir, target); err != nil {
				return err
			}
			if err := ensureSymlinkTargetWithinBase(destDir, target, header.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir for symlink %q: %w", target, err)
			}

			linkTargetPath := filepath.Join(filepath.Dir(target), header.Linkname)
			ok, err := isPathWithinBase(destDir, linkTargetPath)
			if err != nil {
				return fmt.Errorf("validate symlink %q -> %q: %w", target, header.Linkname, err)
			}
			if !ok {
				return fmt.Errorf("reject symlink %q -> %q: target escapes destination", target, header.Linkname)
			}

			_ = os.Remove(target) //nolint:errcheck // overwrite existing

			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("symlink %q -> %q: %w", target, header.Linkname, err)
			}
		default:
			// Skip other entry types (devices, fifos, etc.); the release layout
			// only contains regular files, directories, and symlinks.
		}
	}

	return nil
}

// writeFileFromTar writes a regular file entry to target with the given mode,
// creating parent directories as needed.
func writeFileFromTar(tr io.Reader, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("mkdir for %q: %w", target, err)
	}

	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create %q: %w", target, err)
	}

	if _, err := io.Copy(out, io.LimitReader(tr, maxArchiveBytes)); err != nil {
		_ = out.Close() //nolint:errcheck // best-effort on error path
		return fmt.Errorf("write %q: %w", target, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %q: %w", target, err)
	}

	return nil
}

// stripComponent removes the first path segment from a tar entry name. It
// returns false when the entry has no second segment (i.e. it is the top-level
// directory being stripped).
func stripComponent(name string) (string, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(name), "./")
	clean = strings.TrimPrefix(clean, "/")

	idx := strings.IndexByte(clean, '/')
	if idx < 0 {
		return "", false
	}

	rest := clean[idx+1:]
	if rest == "" {
		return "", false
	}

	return rest, true
}

// safeJoin joins rel onto base, rejecting entries that would escape base via
// absolute paths or "..".
func safeJoin(base, rel string) (string, error) {
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("invalid tar entry name: %q", rel)
	}

	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar entry name: %q", rel)
	}

	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve base path %q: %w", base, err)
	}
	baseAbs = filepath.Clean(baseAbs)

	target := filepath.Clean(filepath.Join(baseAbs, cleaned))
	basePrefix := baseAbs + string(filepath.Separator)
	if target != baseAbs && !strings.HasPrefix(target, basePrefix) {
		return "", fmt.Errorf("unsafe tar entry name: %q", rel)
	}

	return target, nil
}

func ensurePathWithinBase(base, target string) error {
	baseEval, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("resolve base %q: %w", base, err)
	}

	parentEval, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("resolve parent %q: %w", filepath.Dir(target), err)
	}

	candidate := filepath.Join(parentEval, filepath.Base(target))
	if !isWithinBase(baseEval, candidate) {
		return fmt.Errorf("unsafe extraction path: %q", target)
	}

	return nil
}

func ensureSymlinkTargetWithinBase(base, linkPath, linkName string) error {
	baseEval, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("resolve base %q: %w", base, err)
	}

	linkParentEval, err := filepath.EvalSymlinks(filepath.Dir(linkPath))
	if err != nil {
		return fmt.Errorf("resolve symlink parent %q: %w", filepath.Dir(linkPath), err)
	}

	var resolvedTarget string
	if filepath.IsAbs(linkName) {
		resolvedTarget = filepath.Clean(linkName)
	} else {
		resolvedTarget = filepath.Clean(filepath.Join(linkParentEval, filepath.FromSlash(linkName)))
	}

	if !isWithinBase(baseEval, resolvedTarget) {
		return fmt.Errorf("unsafe symlink target %q for link %q", linkName, linkPath)
	}

	return nil
}

func isWithinBase(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
