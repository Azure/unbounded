// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type ImageMetadata struct {
	DHCPBootImageName string `yaml:"dhcpBootImageName"`
	HTTPBootPath      string `yaml:"httpBootPath"`
}

// OCICache manages unpacked OCI images on the local filesystem.
// Images are stored under {cacheDir}/oci/{digest}/{architecture}/disk/...
// This follows the kubevirt containerDisk convention where image
// contents live under /disk/ in the OCI layer.
type OCICache struct {
	CacheDir string

	mu sync.RWMutex
	// image ref and architecture -> digest mapping
	digests map[imageRefKey]string
	// digest and architecture -> metadata mapping
	metadata map[digestKey]*ImageMetadata
}

type imageRefKey struct {
	imageRef     string
	architecture string
}

type digestKey struct {
	digest       string
	architecture string
}

func NewOCICache(cacheDir string) *OCICache {
	return &OCICache{
		CacheDir: cacheDir,
		digests:  make(map[imageRefKey]string),
		metadata: make(map[digestKey]*ImageMetadata),
	}
}

func (c *OCICache) SetDigest(imageRef, digest string) {
	c.SetDigestForArchitecture(imageRef, v1alpha3.DefaultPXEArchitecture, digest)
}

func (c *OCICache) SetDigestForArchitecture(imageRef, architecture, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.digests[imageRefCacheKey(imageRef, architecture)] = digest
}

func (c *OCICache) DigestFor(imageRef string) string {
	return c.DigestForArchitecture(imageRef, v1alpha3.DefaultPXEArchitecture)
}

func (c *OCICache) DigestForArchitecture(imageRef, architecture string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.digests[imageRefCacheKey(imageRef, architecture)]
}

// ImageDir returns the base directory for a cached image by digest.
func (c *OCICache) ImageDir(digest string) string {
	return c.ImageDirForArchitecture(digest, v1alpha3.DefaultPXEArchitecture)
}

// ImageDirForArchitecture returns the base directory for a cached image by
// digest and target architecture.
func (c *OCICache) ImageDirForArchitecture(digest, architecture string) string {
	// Replace ':' with '_' for safe filesystem paths (e.g. "sha256:abc" -> "sha256_abc")
	safe := strings.ReplaceAll(digest, ":", "_")
	return filepath.Join(c.CacheDir, "oci", safe, safeArchitecture(architecture))
}

// DiskDir returns the /disk/ directory for a cached image.
func (c *OCICache) DiskDir(digest string) string {
	return c.DiskDirForArchitecture(digest, v1alpha3.DefaultPXEArchitecture)
}

// DiskDirForArchitecture returns the /disk/ directory for a cached image.
func (c *OCICache) DiskDirForArchitecture(digest, architecture string) string {
	return filepath.Join(c.ImageDirForArchitecture(digest, architecture), "disk")
}

// IsCached returns true if the image digest is already unpacked locally.
func (c *OCICache) IsCached(digest string) bool {
	return c.IsCachedForArchitecture(digest, v1alpha3.DefaultPXEArchitecture)
}

// IsCachedForArchitecture returns true if the image digest is already unpacked locally.
func (c *OCICache) IsCachedForArchitecture(digest, architecture string) bool {
	_, err := os.Stat(c.DiskDirForArchitecture(digest, architecture))
	return err == nil
}

// Metadata returns the parsed metadata.yaml for a cached image,
// reading it from disk and caching in memory on first access.
func (c *OCICache) Metadata(digest string) (*ImageMetadata, error) {
	return c.MetadataForArchitecture(digest, v1alpha3.DefaultPXEArchitecture)
}

// MetadataForArchitecture returns the parsed metadata.yaml for a cached image.
func (c *OCICache) MetadataForArchitecture(digest, architecture string) (*ImageMetadata, error) {
	key := digestCacheKey(digest, architecture)

	c.mu.RLock()

	if m, ok := c.metadata[key]; ok {
		c.mu.RUnlock()
		return m, nil
	}

	c.mu.RUnlock()

	metaPath := filepath.Join(c.DiskDirForArchitecture(digest, architecture), "metadata.yaml")

	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Image has no metadata.yaml - return empty metadata.
			m := &ImageMetadata{}

			c.mu.Lock()
			c.metadata[key] = m
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
	c.metadata[key] = &m
	c.mu.Unlock()

	return &m, nil
}

// MetadataForRef returns the metadata for an image reference by resolving
// its digest first.
func (c *OCICache) MetadataForRef(imageRef string) (*ImageMetadata, error) {
	return c.MetadataForRefArchitecture(imageRef, v1alpha3.DefaultPXEArchitecture)
}

// MetadataForRefArchitecture returns the metadata for an image reference and
// target architecture by resolving its digest first.
func (c *OCICache) MetadataForRefArchitecture(imageRef, architecture string) (*ImageMetadata, error) {
	digest := c.DigestForArchitecture(imageRef, architecture)
	if digest == "" {
		return nil, fmt.Errorf("image %q for architecture %q not yet pulled", imageRef, normalizeArchitecture(architecture))
	}

	return c.MetadataForArchitecture(digest, architecture)
}

// ResolvePath looks for a file at the given path under the disk directory
// for the given image reference. It follows the .tmpl convention:
// if the path doesn't end in .tmpl, it checks for path.tmpl first (template),
// then the path itself (static file).
//
// reqPath must be a relative path with no ".." components that escape the
// cache directory; absolute paths and paths with volume names are rejected.
func (c *OCICache) ResolvePath(imageRef, reqPath string) (diskPath string, isTemplate bool, err error) {
	return c.ResolvePathForArchitecture(imageRef, v1alpha3.DefaultPXEArchitecture, reqPath)
}

// ResolvePathForArchitecture looks for a file at the given path under the disk
// directory for the given image reference and target architecture.
func (c *OCICache) ResolvePathForArchitecture(imageRef, architecture, reqPath string) (diskPath string, isTemplate bool, err error) {
	digest := c.DigestForArchitecture(imageRef, architecture)
	if digest == "" {
		return "", false, fmt.Errorf("image %q for architecture %q not yet pulled", imageRef, normalizeArchitecture(architecture))
	}

	// Reject absolute paths and Windows-style volume names.
	if filepath.IsAbs(reqPath) || filepath.VolumeName(reqPath) != "" {
		return "", false, fmt.Errorf("invalid request path %q: must be relative", reqPath)
	}

	diskDir := c.DiskDirForArchitecture(digest, architecture)

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
	c.InvalidateRefForArchitecture(imageRef, v1alpha3.DefaultPXEArchitecture)
}

// InvalidateRefForArchitecture removes the digest mapping for an image
// reference and target architecture, so it will be re-pulled on next reconcile.
func (c *OCICache) InvalidateRefForArchitecture(imageRef, architecture string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.digests, imageRefCacheKey(imageRef, architecture))
}

func imageRefCacheKey(imageRef, architecture string) imageRefKey {
	return imageRefKey{imageRef: imageRef, architecture: normalizeArchitecture(architecture)}
}

func digestCacheKey(digest, architecture string) digestKey {
	return digestKey{digest: digest, architecture: normalizeArchitecture(architecture)}
}

func normalizeArchitecture(architecture string) string {
	if architecture == "" {
		return v1alpha3.DefaultPXEArchitecture
	}

	return architecture
}

func safeArchitecture(architecture string) string {
	safe := normalizeArchitecture(architecture)
	safe = strings.ReplaceAll(safe, ":", "_")
	safe = strings.ReplaceAll(safe, "/", "_")

	return safe
}
