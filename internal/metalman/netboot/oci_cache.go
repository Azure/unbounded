package netboot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type ImageMetadata struct {
	DHCPBootImageName string `yaml:"dhcpBootImageName"`
}

// OCICache manages unpacked OCI images on the local filesystem.
// Images are stored under {cacheDir}/oci/{digest}/disk/...
// This follows the kubevirt containerDisk convention where image
// contents live under /disk/ in the OCI layer.
type OCICache struct {
	CacheDir string

	mu sync.RWMutex
	// imageRef -> digest mapping
	digests map[string]string
	// digest -> metadata mapping
	metadata map[string]*ImageMetadata
}

func NewOCICache(cacheDir string) *OCICache {
	return &OCICache{
		CacheDir: cacheDir,
		digests:  make(map[string]string),
		metadata: make(map[string]*ImageMetadata),
	}
}

func (c *OCICache) SetDigest(imageRef, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.digests[imageRef] = digest
}

func (c *OCICache) DigestFor(imageRef string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.digests[imageRef]
}

// ImageDir returns the base directory for a cached image by digest.
func (c *OCICache) ImageDir(digest string) string {
	// Replace ':' with '_' for safe filesystem paths (e.g. "sha256:abc" -> "sha256_abc")
	safe := strings.ReplaceAll(digest, ":", "_")
	return filepath.Join(c.CacheDir, "oci", safe)
}

// DiskDir returns the /disk/ directory for a cached image.
func (c *OCICache) DiskDir(digest string) string {
	return filepath.Join(c.ImageDir(digest), "disk")
}

// IsCached returns true if the image digest is already unpacked locally.
func (c *OCICache) IsCached(digest string) bool {
	_, err := os.Stat(c.DiskDir(digest))
	return err == nil
}

// Metadata returns the parsed metadata.yaml for a cached image,
// reading it from disk and caching in memory on first access.
func (c *OCICache) Metadata(digest string) (*ImageMetadata, error) {
	c.mu.RLock()

	if m, ok := c.metadata[digest]; ok {
		c.mu.RUnlock()
		return m, nil
	}

	c.mu.RUnlock()

	metaPath := filepath.Join(c.DiskDir(digest), "metadata.yaml")

	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Image has no metadata.yaml — return empty metadata.
			m := &ImageMetadata{}

			c.mu.Lock()
			c.metadata[digest] = m
			c.mu.Unlock()

			return m, nil
		}

		return nil, fmt.Errorf("reading metadata.yaml: %w", err)
	}

	var m ImageMetadata
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing metadata.yaml: %w", err)
	}

	c.mu.Lock()
	c.metadata[digest] = &m
	c.mu.Unlock()

	return &m, nil
}

// MetadataForRef returns the metadata for an image reference by resolving
// its digest first.
func (c *OCICache) MetadataForRef(imageRef string) (*ImageMetadata, error) {
	digest := c.DigestFor(imageRef)
	if digest == "" {
		return nil, fmt.Errorf("image %q not yet pulled", imageRef)
	}

	return c.Metadata(digest)
}

// ResolvePath looks for a file at the given path under the disk directory
// for the given image reference. It follows the .tmpl convention:
// if the path doesn't end in .tmpl, it checks for path.tmpl first (template),
// then the path itself (static file).
//
// reqPath must be a relative path with no ".." components that escape the
// cache directory; absolute paths and paths with volume names are rejected.
func (c *OCICache) ResolvePath(imageRef, reqPath string) (diskPath string, isTemplate bool, err error) {
	digest := c.DigestFor(imageRef)
	if digest == "" {
		return "", false, fmt.Errorf("image %q not yet pulled", imageRef)
	}

	// Reject absolute paths and Windows-style volume names.
	if filepath.IsAbs(reqPath) || filepath.VolumeName(reqPath) != "" {
		return "", false, fmt.Errorf("invalid request path %q: must be relative", reqPath)
	}

	diskDir := c.DiskDir(digest)

	// Resolve and clean the joined path, then ensure it is still rooted under
	// diskDir. filepath.Clean eliminates any ".." segments, so a traversal
	// attempt such as "../../etc/passwd" will be caught by the prefix check.
	// We require a non-empty suffix after diskDir (i.e. cleanedBase must be a
	// file/directory *inside* diskDir, not diskDir itself).
	cleanedBase := filepath.Clean(filepath.Join(diskDir, reqPath))
	prefix := diskDir + string(filepath.Separator)

	if !strings.HasPrefix(cleanedBase, prefix) {
		return "", false, fmt.Errorf("invalid request path %q: resolves outside cache directory", reqPath)
	}

	// Check for template version first (reqPath + ".tmpl")
	tmplPath := cleanedBase + ".tmpl"
	if _, err := os.Stat(tmplPath); err == nil {
		return tmplPath, true, nil
	}

	// Check for static file
	if _, err := os.Stat(cleanedBase); err == nil {
		return cleanedBase, false, nil
	}

	return "", false, fmt.Errorf("file not found in image %q: %s", imageRef, reqPath)
}

// InvalidateRef removes the digest mapping for an image reference,
// so it will be re-pulled on next reconcile.
func (c *OCICache) InvalidateRef(imageRef string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.digests, imageRef)
}
