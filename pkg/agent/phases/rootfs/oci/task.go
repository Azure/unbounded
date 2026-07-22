// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	ociLayoutScheme    = "oci-layout://"
	ociImageScheme     = "oci://"
	httpsImageScheme   = "https://"
	ociPullMaxAttempts = 5
	ociPullRetryDelay  = 2 * time.Second
)

type downloadRootFS struct {
	log        *slog.Logger
	machineDir string
	ociImage   string
	hostArch   string
}

// DownloadRootFS downloads an OCI image and unpacks it into the machine
// directory as rootfs.
func DownloadRootFS(
	log *slog.Logger,
	machineDir string,
	hostArch string,
	ociImage string,
) phases.Task {
	return &downloadRootFS{
		log:        log,
		machineDir: machineDir,
		ociImage:   ociImage,
		hostArch:   hostArch,
	}
}

func (d *downloadRootFS) Name() string { return "oci-download-rootfs" }

func (d *downloadRootFS) Do(ctx context.Context) error {
	empty, err := utilio.IsDirEmpty(d.machineDir)
	if err != nil {
		return fmt.Errorf("check machine directory %s: %w", d.machineDir, err)
	}

	if !empty {
		d.log.Warn("machine directory is not empty, skipping rootfs bootstrap", slog.String("dir", d.machineDir))
		return nil
	}

	if archiveURL, ok, err := parseHTTPSArchiveReference(d.ociImage); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse HTTPS OCI archive reference %q: %w", d.ociImage, err)
		}

		return d.downloadArchiveAndUnpack(ctx, archiveURL)
	}

	if layoutDir, tag, ok, err := parseOCILayoutReference(d.ociImage); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse OCI layout reference %q: %w", d.ociImage, err)
		}

		d.log.Info("using local OCI layout image",
			slog.String("image", d.ociImage),
			slog.String("layout", layoutDir),
			slog.String("dest", d.machineDir))

		if err := os.MkdirAll(d.machineDir, 0o755); err != nil {
			return fmt.Errorf("create machine directory: %w", err)
		}

		if err := unpackOCILayout(ctx, d.log, d.hostArch, layoutDir, tag, d.machineDir); err != nil {
			return fmt.Errorf("unpack OCI layout image: %w", err)
		}

		d.log.Info("OCI image extraction complete",
			slog.String("dest", d.machineDir))

		return nil
	}

	// Parse the image reference into registry/repository and tag components.
	ref, tag, err := parseImageReference(d.ociImage)
	if err != nil {
		return fmt.Errorf("parse image reference %q: %w", d.ociImage, err)
	}

	d.log.Info("pulling OCI image",
		slog.String("image", d.ociImage),
		slog.String("dest", d.machineDir))

	// Pull the image into a temporary OCI layout store, then use umoci to
	// unpack the layers into the machine directory.
	return d.pullAndUnpack(ctx, ref, tag)
}

func (d *downloadRootFS) downloadArchiveAndUnpack(ctx context.Context, archiveURL string) error {
	layoutParent, err := os.MkdirTemp("", "unbounded-oci-archive-*")
	if err != nil {
		return fmt.Errorf("create OCI archive extraction directory: %w", err)
	}
	defer os.RemoveAll(layoutParent) //nolint:errcheck // best effort cleanup

	source, err := artifactsource.Parse(archiveURL)
	if err != nil {
		return fmt.Errorf("parse HTTPS OCI archive source: %w", err)
	}

	d.log.Info("downloading HTTPS OCI image archive",
		slog.String("image", archiveURL),
		slog.String("dest", d.machineDir))

	if err := source.ExtractTar(ctx, layoutParent); err != nil {
		return fmt.Errorf("download and extract HTTPS OCI image archive: %w", err)
	}

	layoutDir, err := findOCILayoutRoot(layoutParent)
	if err != nil {
		return err
	}

	tag, err := singleOCILayoutReference(layoutDir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(d.machineDir, 0o755); err != nil {
		return fmt.Errorf("create machine directory: %w", err)
	}

	if err := unpackOCILayout(ctx, d.log, d.hostArch, layoutDir, tag, d.machineDir); err != nil {
		return fmt.Errorf("unpack HTTPS OCI image archive: %w", err)
	}

	d.log.Info("OCI image extraction complete", slog.String("dest", d.machineDir))

	return nil
}

// pullAndUnpack pulls the OCI image into a temporary OCI layout directory and
// unpacks it into the machine directory using umoci.
func (d *downloadRootFS) pullAndUnpack(ctx context.Context, ref, tag string) error {
	// Create a temporary directory for the OCI layout store.
	layoutDir, err := os.MkdirTemp("", "unbounded-oci-*")
	if err != nil {
		return fmt.Errorf("create temp dir for OCI layout: %w", err)
	}
	defer os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup

	store, err := oci.New(layoutDir)
	if err != nil {
		return fmt.Errorf("create OCI layout store: %w", err)
	}

	repo, err := newRemoteRepository(ref)
	if err != nil {
		return fmt.Errorf("connect to remote repository %q: %w", ref, err)
	}

	// Copy (pull) the image from the remote repository into the local OCI layout.
	desc, err := oras.Copy(ctx, repo, tag, store, tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("pull image %s:%s: %w", ref, tag, err)
	}

	d.log.Info("pulled image manifest",
		slog.String("digest", desc.Digest.String()),
		slog.String("mediaType", desc.MediaType))

	// Unpack the OCI layout into the machine directory.
	if err := os.MkdirAll(d.machineDir, 0o755); err != nil {
		return fmt.Errorf("create machine directory: %w", err)
	}

	if err := unpackOCILayout(ctx, d.log, d.hostArch, layoutDir, tag, d.machineDir); err != nil {
		return fmt.Errorf("unpack OCI image: %w", err)
	}

	d.log.Info("OCI image extraction complete",
		slog.String("dest", d.machineDir))

	return nil
}

func newRemoteRepository(ref string) (*remote.Repository, error) {
	// Connect to the remote repository. We assume public access (no auth).
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, err
	}

	// Use plain HTTP for loopback and private-network registries.
	ociutil.ConfigurePlainHTTP(repo)
	configureOCIPullRetry(repo)

	return repo, nil
}

func configureOCIPullRetry(repo *remote.Repository) {
	repo.Client = &auth.Client{
		Client: &http.Client{
			Transport: &retry.Transport{
				Policy: func() retry.Policy {
					return newOCIPullRetryPolicy()
				},
			},
		},
		Header: auth.DefaultClient.Header.Clone(),
		Cache:  auth.DefaultCache,
	}
}

func newOCIPullRetryPolicy() retry.Policy {
	return &retry.GenericPolicy{
		Retryable: retryOCIPullFailure,
		Backoff:   ociPullBackoff,
		MinWait:   ociPullRetryDelay,
		MaxWait:   maxOCIPullRetryDelay(),
		MaxRetry:  ociPullMaxAttempts - 1,
	}
}

func retryOCIPullFailure(resp *http.Response, err error) (bool, error) {
	if retryableOCIPullTransportError(err) {
		return true, nil
	}

	if resp == nil {
		return false, nil
	}

	return retry.DefaultPredicate(resp, nil)
}

// ORAS's default retry policy already handles retryable HTTP statuses, but it
// only retries transport errors that are timeouts. Agent bootstrap can also hit
// transient DNS and connectivity failures before the registry returns an HTTP
// response, so include those selected network errors without retrying every
// transport failure.
func retryableOCIPullTransportError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	for _, target := range []error{
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}

func ociPullBackoff(attempt int, _ *http.Response) time.Duration {
	delay := ociPullRetryDelay
	for range attempt {
		delay *= 2
	}

	return delay
}

func maxOCIPullRetryDelay() time.Duration {
	delay := ociPullRetryDelay
	for range ociPullMaxAttempts - 2 {
		delay *= 2
	}

	return delay
}

// CheckImageReachable validates that an OCI registry manifest, local layout,
// or HTTPS OCI layout archive is reachable without pulling image contents.
func CheckImageReachable(ctx context.Context, image string) error {
	if archiveURL, ok, err := parseHTTPSArchiveReference(image); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse HTTPS OCI archive reference: %w", err)
		}

		source, err := artifactsource.Parse(archiveURL)
		if err != nil {
			return fmt.Errorf("parse HTTPS OCI archive source: %w", err)
		}

		if err := source.Probe(ctx); err != nil {
			return fmt.Errorf("probe HTTPS OCI archive: %w", err)
		}

		return nil
	}

	if layoutDir, tag, ok, err := parseOCILayoutReference(image); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse OCI layout reference: %w", err)
		}

		store, err := oci.New(layoutDir)
		if err != nil {
			return fmt.Errorf("open OCI layout: %w", err)
		}

		if _, err := store.Resolve(ctx, tag); err != nil {
			return fmt.Errorf("resolve OCI layout image manifest: %w", err)
		}

		return nil
	}

	ref, tag, err := parseImageReference(image)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	repo, err := newRemoteRepository(ref)
	if err != nil {
		return fmt.Errorf("connect to remote repository: %w", err)
	}

	if _, err := repo.Resolve(ctx, tag); err != nil {
		return fmt.Errorf("resolve image manifest: %w", err)
	}

	return nil
}

func parseHTTPSArchiveReference(image string) (archiveURL string, ok bool, err error) {
	if !strings.HasPrefix(image, httpsImageScheme) {
		return "", false, nil
	}

	parsed, err := url.Parse(image)
	if err != nil {
		return "", true, err
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return "", true, fmt.Errorf("HTTPS OCI archive reference must include a host and archive path")
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", true, fmt.Errorf("HTTPS OCI archive reference must not include user info, query parameters, or a fragment")
	}

	return parsed.String(), true, nil
}

func findOCILayoutRoot(extractDir string) (string, error) {
	var layouts []string

	if err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() && entry.Name() == "blobs" {
			return filepath.SkipDir
		}

		if !entry.IsDir() && entry.Name() == "oci-layout" {
			layouts = append(layouts, filepath.Dir(path))
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("inspect HTTPS OCI image archive: %w", err)
	}

	if len(layouts) == 0 {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain an OCI layout")
	}

	if len(layouts) > 1 {
		return "", fmt.Errorf("HTTPS OCI image archive contains multiple OCI layouts")
	}

	indexInfo, err := os.Stat(filepath.Join(layouts[0], "index.json"))
	if err != nil || !indexInfo.Mode().IsRegular() {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain a regular index.json")
	}

	return layouts[0], nil
}

func singleOCILayoutReference(layoutDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return "", fmt.Errorf("read HTTPS OCI image archive index: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return "", fmt.Errorf("parse HTTPS OCI image archive index: %w", err)
	}

	var reference string

	for _, descriptor := range index.Manifests {
		name := descriptor.Annotations[ocispec.AnnotationRefName]
		if name == "" {
			continue
		}

		if reference != "" {
			return "", fmt.Errorf("HTTPS OCI image archive contains multiple tagged image references")
		}

		reference = name
	}

	if reference == "" {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain a tagged image reference")
	}

	return reference, nil
}

func parseOCILayoutReference(image string) (layoutDir, tag string, ok bool, err error) {
	if !strings.HasPrefix(image, ociLayoutScheme) {
		return "", "", false, nil
	}

	source := strings.TrimPrefix(image, ociLayoutScheme)
	if source == "" {
		return "", "", true, fmt.Errorf("empty OCI layout reference")
	}

	lastSlash := strings.LastIndex(source, "/")
	lastColon := strings.LastIndex(source, ":")

	if lastColon > lastSlash {
		layoutDir = source[:lastColon]
		tag = source[lastColon+1:]

		if layoutDir == "" || tag == "" {
			return "", "", true, fmt.Errorf("invalid OCI layout reference")
		}

		return layoutDir, tag, true, nil
	}

	return source, "latest", true, nil
}

// parseImageReference splits an OCI image reference like
// "registry.example.com/repo:tag" or "oci://registry.example.com/repo:tag"
// into the repository reference and tag. If no tag is specified, "latest" is
// used.
func parseImageReference(image string) (ref, tag string, err error) {
	image = strings.TrimPrefix(image, ociImageScheme)

	if image == "" {
		return "", "", fmt.Errorf("empty image reference")
	}

	// Handle digest references (e.g., repo@sha256:abc123).
	if idx := strings.LastIndex(image, "@"); idx != -1 {
		return image[:idx], image[idx+1:], nil
	}

	// Split off the tag. We need to be careful not to split on colons
	// that are part of the registry (e.g., localhost:5000/repo:tag).
	// The tag is the part after the last colon that comes after the last slash.
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")

	if lastColon > lastSlash && lastColon != -1 {
		return image[:lastColon], image[lastColon+1:], nil
	}

	// No tag specified, default to "latest".
	return image, "latest", nil
}
