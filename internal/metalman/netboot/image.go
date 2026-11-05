// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
)

// ErrImageUnavailable reports that an image could not be read from its
// registry, for example because the registry is unreachable, rejects the
// credentials, or does not have the image yet. Callers should treat it as
// transient and retry.
var ErrImageUnavailable = errors.New("image unavailable")

// imageDiskDir is the directory that holds boot artifacts inside an image,
// following the kubevirt containerDisk convention.
const imageDiskDir = "disk"

// metadataFile is the image metadata file, relative to imageDiskDir.
const metadataFile = "metadata.yaml"

// ImageMetadata is the content of disk/metadata.yaml in a netboot image.
type ImageMetadata struct {
	DHCPBootImageName string `yaml:"dhcpBootImageName"`
	HTTPBootPath      string `yaml:"httpBootPath"`
	// MediaFormat gates the scoped installer callback and bootstrap layout.
	MediaFormat string `yaml:"mediaFormat"`

	// missing reports that the image has no metadata.yaml. Such results are
	// memoized only for the short failure TTL.
	missing bool
}

// ImageSource identifies an image that boot artifacts are read from, together
// with the pull secret that grants access to it.
type ImageSource struct {
	Ref           string
	Architecture  string
	PullSecretRef *v1alpha3.NamespacedSecretReference
}

func (s ImageSource) architecture() string {
	return normalizeArchitecture(s.Architecture)
}

func normalizeArchitecture(architecture string) string {
	if architecture == "" {
		return v1alpha3.DefaultPXEArchitecture
	}

	return architecture
}

// NetbootImageSource returns the image that holds the network boot artifacts
// (boot loaders, kernel, initrd, and templates) for node. An explicit
// netboot image wins, then the configured default netboot image, then the
// Machine's OS image. The pull secret follows the selected image.
func (f *FileResolver) NetbootImageSource(node *v1alpha3.Machine) ImageSource {
	if node == nil || node.Spec.Netboot() == nil {
		return ImageSource{}
	}

	spec := node.Spec.Netboot()
	arch := spec.TargetArchitecture()

	switch {
	case spec.NetbootImage != "":
		return ImageSource{Ref: spec.NetbootImage, Architecture: arch, PullSecretRef: spec.NetbootPullSecretRef}
	case f.DefaultNetbootRef != "":
		return ImageSource{Ref: f.DefaultNetbootRef, Architecture: arch, PullSecretRef: f.DefaultNetbootPullSecretRef}
	default:
		return ImageSource{Ref: spec.Image, Architecture: arch, PullSecretRef: spec.PullSecretRef}
	}
}

// DiskImageSource returns the image that holds the OS disk image for node.
func DiskImageSource(node *v1alpha3.Machine) ImageSource {
	if node == nil || node.Spec.Netboot() == nil {
		return ImageSource{}
	}

	spec := node.Spec.Netboot()

	return ImageSource{Ref: spec.Image, Architecture: spec.TargetArchitecture(), PullSecretRef: spec.PullSecretRef}
}

// mediaNetbootSource and mediaDiskSource return the frozen image sources of
// an operation-owned boot media snapshot.
func mediaNetbootSource(spec *v1alpha3.PXESpec) ImageSource {
	return ImageSource{Ref: spec.NetbootImage, Architecture: spec.TargetArchitecture(), PullSecretRef: spec.NetbootPullSecretRef}
}

func mediaDiskSource(spec *v1alpha3.PXESpec) ImageSource {
	return ImageSource{Ref: spec.Image, Architecture: spec.TargetArchitecture(), PullSecretRef: spec.PullSecretRef}
}

// resolveImage resolves src to a pinned image, reading its pull secret first.
// Every failure wraps ErrImageUnavailable.
func (f *FileResolver) resolveImage(ctx context.Context, src ImageSource) (*imagepull.Image, error) {
	if src.Ref == "" {
		return nil, fmt.Errorf("%w: no image reference", ErrImageUnavailable)
	}

	if f.Puller == nil {
		return nil, fmt.Errorf("%w: image puller is not configured", ErrImageUnavailable)
	}

	cred, err := pullCredential(ctx, f.Reader, src.Ref, src.PullSecretRef)
	if err != nil {
		return nil, fmt.Errorf("%w: image %q: %w", ErrImageUnavailable, src.Ref, err)
	}

	img, err := f.Puller.Resolve(ctx, imagepull.Source{Ref: src.Ref, Architecture: src.architecture(), Credential: cred})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageUnavailable, err)
	}

	return img, nil
}

// imagePaths validates reqPath and returns the template and static entry
// names for it inside the image. reqPath must be relative and must stay
// inside the disk directory.
func imagePaths(reqPath string) (tmpl, static string, err error) {
	if path.IsAbs(reqPath) || filepath.IsAbs(reqPath) || filepath.VolumeName(reqPath) != "" {
		return "", "", fmt.Errorf("invalid request path %q: must be relative", reqPath)
	}

	cleaned := path.Clean(path.Join(imageDiskDir, reqPath))
	if !strings.HasPrefix(cleaned, imageDiskDir+"/") {
		return "", "", fmt.Errorf("invalid request path %q: resolves outside the image disk directory", reqPath)
	}

	return cleaned + ".tmpl", cleaned, nil
}

// openImageFile opens reqPath.tmpl or reqPath from the image in a single pass
// over its layers. Layers are searched top-down, so a template in an upper
// layer takes precedence over a static file in a lower layer and the other
// way around. If both are in the same layer, the first tar entry wins; image
// builds should not ship both. Errors from the registry wrap
// ErrImageUnavailable; a missing file wraps imagepull.ErrNotFound.
func (f *FileResolver) openImageFile(ctx context.Context, src ImageSource, reqPath string) (*imagepull.File, bool, error) {
	if _, _, err := imagePaths(reqPath); err != nil {
		return nil, false, err
	}

	img, err := f.resolveImage(ctx, src)
	if err != nil {
		return nil, false, err
	}

	return openFileInImage(ctx, img, reqPath)
}

// openFileInImage is openImageFile for an already resolved image.
func openFileInImage(ctx context.Context, img *imagepull.Image, reqPath string) (*imagepull.File, bool, error) {
	tmplName, staticName, err := imagePaths(reqPath)
	if err != nil {
		return nil, false, err
	}

	file, err := img.Open(ctx, tmplName, staticName)
	if err != nil {
		if errors.Is(err, imagepull.ErrNotFound) {
			return nil, false, fmt.Errorf("file not found in image %q: %s: %w", img.Source().Ref, reqPath, err)
		}

		return nil, false, fmt.Errorf("%w: %w", ErrImageUnavailable, err)
	}

	return file, file.Name() == tmplName, nil
}

// readTemplateFile reads a template opened by openImageFile, bounded by
// imagepull.MaxReadFileSize, and closes it.
func readTemplateFile(file *imagepull.File) ([]byte, error) {
	defer file.Close() //nolint:errcheck // read-only

	if file.Size() > imagepull.MaxReadFileSize {
		return nil, fmt.Errorf("template %s is %d bytes, larger than the %d byte limit", file.Name(), file.Size(), imagepull.MaxReadFileSize)
	}

	// Read through EOF rather than exactly Size bytes: File.Read reports a
	// content digest mismatch only at EOF, possibly together with the final
	// bytes, and io.ReadFull would drop that error once the buffer is full.
	data, err := io.ReadAll(io.LimitReader(file, file.Size()+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrImageUnavailable, file.Name(), err)
	}

	if int64(len(data)) != file.Size() {
		return nil, fmt.Errorf("%w: reading %s: got %d bytes, want %d", ErrImageUnavailable, file.Name(), len(data), file.Size())
	}

	return data, nil
}

// fetchMetadata reads and parses disk/metadata.yaml from src. An image
// without metadata.yaml has empty metadata.
func (f *FileResolver) fetchMetadata(ctx context.Context, src ImageSource) (*ImageMetadata, error) {
	img, err := f.resolveImage(ctx, src)
	if err != nil {
		return nil, err
	}

	return readImageMetadata(ctx, img)
}

// readImageMetadata reads and parses disk/metadata.yaml from img.
func readImageMetadata(ctx context.Context, img *imagepull.Image) (*ImageMetadata, error) {
	data, _, err := img.ReadFile(ctx, path.Join(imageDiskDir, metadataFile))
	if err != nil {
		if errors.Is(err, imagepull.ErrNotFound) {
			return &ImageMetadata{missing: true}, nil
		}

		return nil, fmt.Errorf("%w: reading %s: %w", ErrImageUnavailable, metadataFile, err)
	}

	var m ImageMetadata
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s in %q: %w", metadataFile, img.Source().Ref, err)
	}

	return &m, nil
}

// ImageMetadata returns the parsed metadata of src. When the resolver has a
// MetadataMemo, results are memoized briefly; image content is never cached.
func (f *FileResolver) ImageMetadata(ctx context.Context, src ImageSource) (*ImageMetadata, error) {
	if f.Metadata == nil {
		return f.fetchMetadata(ctx, src)
	}

	return f.Metadata.get(ctx, src, f.fetchMetadata)
}

// ImageMetadataOrStale is ImageMetadata for DHCP replies. When the lookup
// fails or ctx expires, it falls back to the last successful metadata for src
// (see MetadataMemo.StaleTTL) and reports stale with the lookup error. Without
// a MetadataMemo it behaves like ImageMetadata.
func (f *FileResolver) ImageMetadataOrStale(ctx context.Context, src ImageSource) (meta *ImageMetadata, stale bool, err error) {
	if f.Metadata == nil {
		meta, err = f.fetchMetadata(ctx, src)
		return meta, false, err
	}

	return f.Metadata.getOrStale(ctx, src, f.fetchMetadata)
}

// PinnedImageMetadata returns the parsed metadata of an already resolved
// image. Results are memoized by manifest digest, so they always describe
// img itself even after the tag it was resolved from moves.
func (f *FileResolver) PinnedImageMetadata(ctx context.Context, img *imagepull.Image) (*ImageMetadata, error) {
	if f.Metadata == nil {
		return readImageMetadata(ctx, img)
	}

	return f.Metadata.getPinned(ctx, img.Digest(), func(ctx context.Context) (*ImageMetadata, error) {
		return readImageMetadata(ctx, img)
	})
}
