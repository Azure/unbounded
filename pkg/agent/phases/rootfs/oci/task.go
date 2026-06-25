// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
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

	// Connect to the remote repository. We assume public access (no auth).
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return fmt.Errorf("connect to remote repository %q: %w", ref, err)
	}

	// Use plain HTTP for loopback and private-network registries.
	ociutil.ConfigurePlainHTTP(repo)
	configureOCIPullRetry(repo)

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

// parseImageReference splits an OCI image reference like
// "registry.example.com/repo:tag" into the repository reference and tag.
// If no tag is specified, "latest" is used.
func parseImageReference(image string) (ref, tag string, err error) {
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
