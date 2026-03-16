package unboundedcni

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const defaultCacheDir = ".unbounded-forge/unbounded-cni"

// releaseURLPattern extracts the version from the release URL.
// Expected format: .../releases/download/<version>/unbounded-cni-manifests-<version>.tar.gz
var releaseURLPattern = regexp.MustCompile(`/releases/download/(v[^/]+)/`)

// ParseVersionFromURL extracts the version string from an unbounded-cni release URL.
func ParseVersionFromURL(releaseURL string) (string, error) {
	matches := releaseURLPattern.FindStringSubmatch(releaseURL)
	if len(matches) < 2 {
		return "", fmt.Errorf("unable to parse version from URL: %s", releaseURL)
	}

	return matches[1], nil
}

// CacheDir returns the cache directory for unbounded-cni manifests of a given version.
// The directory is located at ~/.unbounded-forge/unbounded-cni/<version>/.
func CacheDir(version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get user home directory: %w", err)
	}

	return filepath.Join(home, defaultCacheDir, version), nil
}

// localCacheDir returns the cache directory for locally-sourced unbounded-cni manifests.
// The directory is located at ~/.unbounded-forge/unbounded-cni/local/ and is always refreshed.
func localCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get user home directory: %w", err)
	}

	return filepath.Join(home, defaultCacheDir, "local"), nil
}

// DownloadAndCache downloads the unbounded-cni manifests tarball from the given URL,
// extracts it to the cache directory, and returns the path to the cached manifests.
// For https:// URLs, manifests are cached by version and reused on subsequent calls.
// For file:// URLs, manifests are always extracted fresh to a local cache directory.
func DownloadAndCache(logger *slog.Logger, releaseURL string) (string, error) {
	parsedURL, err := url.Parse(releaseURL)
	if err != nil {
		return "", fmt.Errorf("parse release URL: %w", err)
	}

	switch parsedURL.Scheme {
	case "file":
		return extractFromLocal(logger, parsedURL.Path)
	case "https":
		return downloadFromRemote(logger, releaseURL)
	default:
		return "", fmt.Errorf("unsupported URL scheme %q: expected https:// or file://", parsedURL.Scheme)
	}
}

// extractFromLocal extracts a local tarball to the local cache directory, always refreshing.
func extractFromLocal(logger *slog.Logger, path string) (string, error) {
	cacheDir, err := localCacheDir()
	if err != nil {
		return "", err
	}

	// Always extract fresh — remove any previous contents.
	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("remove local cache directory: %w", err)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create local cache directory: %w", err)
	}

	logger.Info("Extracting local unbounded-cni manifests", "path", path, "dest", cacheDir)

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open local tarball %s: %w", path, err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close file: %v\n", err)
		}
	}()

	if err := extractTarGz(f, cacheDir); err != nil {
		// Clean up partial extraction.
		_ = os.RemoveAll(cacheDir) //nolint:errcheck // Best-effort cleanup of partial extraction.
		return "", fmt.Errorf("extract local tarball: %w", err)
	}

	logger.Info("Extracted local unbounded-cni manifests", "path", cacheDir)

	return cacheDir, nil
}

// downloadFromRemote downloads and caches manifests from an HTTPS URL.
// If the manifests are already cached, it returns the cached path without downloading.
func downloadFromRemote(logger *slog.Logger, releaseURL string) (string, error) {
	version, err := ParseVersionFromURL(releaseURL)
	if err != nil {
		return "", err
	}

	cacheDir, err := CacheDir(version)
	if err != nil {
		return "", err
	}

	// Check if already cached.
	if info, err := os.Stat(cacheDir); err == nil && info.IsDir() {
		entries, err := os.ReadDir(cacheDir)
		if err == nil && len(entries) > 0 {
			logger.Info("Using cached unbounded-cni manifests", "version", version, "path", cacheDir)
			return cacheDir, nil
		}
	}

	logger.Info("Downloading unbounded-cni manifests", "version", version, "url", releaseURL)

	if err := downloadAndExtract(releaseURL, cacheDir); err != nil {
		// Clean up partial download.
		_ = os.RemoveAll(cacheDir) //nolint:errcheck // Best-effort cleanup of partial download.
		return "", fmt.Errorf("download and extract unbounded-cni manifests: %w", err)
	}

	logger.Info("Cached unbounded-cni manifests", "version", version, "path", cacheDir)

	return cacheDir, nil
}

func downloadAndExtract(releaseURL, destDir string) error {
	resp, err := http.Get(releaseURL) //nolint:gosec,noctx // URL is validated by caller and comes from trusted configuration.
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %d when downloading %s", resp.StatusCode, releaseURL)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	return extractTarGz(resp.Body, destDir)
}

func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}

	defer func() {
		if err := gz.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close gzip reader: %v\n", err)
		}
	}()

	tr := tar.NewReader(gz)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// Prevent path traversal attacks.
		name := filepath.Clean(header.Name)
		if strings.Contains(name, "..") {
			return fmt.Errorf("tar entry contains path traversal: %s", header.Name)
		}

		target := filepath.Join(destDir, name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create directory %s: %w", target, err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent directory for %s: %w", target, err)
			}

			if err := writeFile(target, tr, header.FileInfo().Mode()); err != nil {
				return fmt.Errorf("write file %s: %w", target, err)
			}

		default:
			// Skip non-regular files (symlinks, etc.)
			continue
		}
	}
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close() //nolint:errcheck // Best-effort close on copy failure.
		return err
	}

	return f.Close()
}

// OverrideControllerImage replaces the controller container image in the unbounded-cni
// controller deployment manifest (controller/03-deploy.yaml).
func OverrideControllerImage(logger *slog.Logger, manifestDir, image string) error {
	return overrideImageInFile(logger, filepath.Join(manifestDir, "controller", "03-deploy.yaml"), "controller", image)
}

// OverrideNodeImage replaces the node container image in the unbounded-cni
// node daemonset manifest (node/03-daemonset.yaml).
func OverrideNodeImage(logger *slog.Logger, manifestDir, image string) error {
	return overrideImageInFile(logger, filepath.Join(manifestDir, "node", "03-daemonset.yaml"), "node", image)
}

// overrideImageInFile replaces the first image: reference in a specific manifest file.
func overrideImageInFile(logger *slog.Logger, path, component, image string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", path, err)
	}

	content := string(data)

	// Match the image: field in Kubernetes manifests.
	pattern := regexp.MustCompile(`(image:\s*)\S+`)

	if !pattern.MatchString(content) {
		return fmt.Errorf("no image reference found in %s", path)
	}

	modified := pattern.ReplaceAllString(content, "${1}"+image)

	logger.Info("Overriding image in manifest", "file", path, "component", component, "image", image)

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat manifest %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(modified), info.Mode()); err != nil {
		return fmt.Errorf("write modified manifest %s: %w", path, err)
	}

	return nil
}

// PatchConfigMapTenantID patches the Azure tenant ID in unbounded-cni ConfigMap manifests.
// It looks for files containing azureTenantId: "" and replaces the empty value with the
// provided tenant ID.
func PatchConfigMapTenantID(logger *slog.Logger, manifestDir, tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("tenant ID is empty")
	}

	return filepath.Walk(manifestDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", path, err)
		}

		content := string(data)

		const placeholder = `azureTenantId: ""`
		if !strings.Contains(content, placeholder) {
			return nil
		}

		replacement := fmt.Sprintf(`azureTenantId: "%s"`, tenantID)
		modified := strings.ReplaceAll(content, placeholder, replacement)

		logger.Info("Patched tenant ID in configmap", "file", path, "tenantID", tenantID)

		if err := os.WriteFile(path, []byte(modified), info.Mode()); err != nil {
			return fmt.Errorf("write patched manifest %s: %w", path, err)
		}

		return nil
	})
}
