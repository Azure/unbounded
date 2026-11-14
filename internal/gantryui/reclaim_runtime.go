// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/shirou/gopsutil/v4/disk"
)

// ReclaimNodeResult reports image identities removed, not individual aliases.
// Before and After are filesystem used bytes, not attributable image sizes.
type ReclaimNodeResult struct {
	Node            string `json:"node"`
	Images          int    `json:"images"`
	Leases          int    `json:"leases"`
	ProtectedLeases int    `json:"protectedLeases"`
	Before          uint64 `json:"before"`
	After           uint64 `json:"after"`
}

type reclaimImage struct {
	name   string
	target ocispec.Descriptor
	config string
	pinned bool
}

// No container, snapshot, content, or lease mutation is exposed here.
type reclaimRuntime interface {
	listImages(context.Context) ([]reclaimImage, error)
	listContainerImages(context.Context) ([]string, error)
	countLeases(context.Context) (int, error)
	deleteImage(context.Context, reclaimImage) error
}

type reclaimContainerd struct {
	client *containerd.Client
}

// ReclaimNode removes unused image records in containerd's Kubernetes namespace.
// It never stops or deletes containers, including stopped containers. Containerd
// garbage collection, not this function, decides which backing resources are
// unreachable. All runtime leases are retained: neither demo content digests,
// lease age, nor a lack of ingests proves that a pull or unpack has finished.
// ProtectedLeases counts all retained leases, including sandbox leases; callers
// should explain that these may retain data after successful image removal.
//
// demoDigests identifies deleted demo manifests whose image records must disappear
// before success. It grants no authority to remove containers or runtime leases.
func ReclaimNode(ctx context.Context, node, socket, storageRoot string, demoDigests []string) (ReclaimNodeResult, error) {
	result := ReclaimNodeResult{Node: node}
	if node == "" || socket == "" || storageRoot == "" {
		return result, errors.New("reclaim requires a node, containerd socket, and storage root")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := containerd.New(socket, containerd.WithTimeout(10*time.Second))
	if err != nil {
		return result, fmt.Errorf("connect to containerd: %w", err)
	}
	defer func() { _ = client.Close() }() //nolint:errcheck // Closing the local RPC connection does not affect completed cleanup.

	return reclaimNode(namespaces.WithNamespace(ctx, "k8s.io"), result, reclaimContainerd{client: client}, func(ctx context.Context) (uint64, error) {
		usage, err := disk.UsageWithContext(ctx, storageRoot)
		if err != nil {
			return 0, err
		}

		return usage.Used, nil
	}, demoDigests...)
}

func reclaimNode(ctx context.Context, result ReclaimNodeResult, runtime reclaimRuntime, used func(context.Context) (uint64, error), demoDigests ...string) (ReclaimNodeResult, error) {
	before, err := used(ctx)
	if err != nil {
		return result, fmt.Errorf("measure storage before reclaim: %w", err)
	}

	result.Before, result.After = before, before

	result.ProtectedLeases, err = runtime.countLeases(ctx)
	if err != nil {
		return result, fmt.Errorf("list protected runtime leases: %w", err)
	}

	work, err := runtime.listImages(ctx)
	if err != nil {
		return result, fmt.Errorf("list image identities: %w", err)
	}

	seen := make(map[string]bool)

	var failures []error

	for _, image := range work {
		if seen[image.config] {
			continue
		}

		seen[image.config] = true

		removed, err := reclaimImageIdentity(ctx, runtime, image.config)
		if err != nil {
			failures = append(failures, err)
			break
		}

		if removed {
			result.Images++
		}
	}

	if len(failures) == 0 && len(demoDigests) > 0 {
		removed, err := reclaimDeletedDemoImages(ctx, runtime, demoDigests)
		result.Images += removed

		if err != nil {
			failures = append(failures, err)
		}
	}

	after, err := used(ctx)
	if err != nil {
		failures = append(failures, fmt.Errorf("measure storage after reclaim: %w", err))
	} else {
		result.After = after
	}

	return result, errors.Join(failures...)
}

func reclaimDeletedDemoImages(ctx context.Context, runtime reclaimRuntime, digests []string) (int, error) {
	targets := make(map[string]bool, len(digests))
	for _, digest := range digests {
		targets[digest] = true
	}

	identities := make(map[string]bool)
	removed := 0

	for {
		current, err := runtime.listImages(ctx)
		if err != nil {
			return removed, fmt.Errorf("verify deleted demo images: %w", err)
		}

		for _, image := range current {
			if targets[image.target.Digest.String()] {
				identities[image.config] = true
			}
		}

		pending := make(map[string]bool)

		for _, image := range current {
			if identities[image.config] {
				pending[image.config] = true
				if image.pinned {
					return removed, fmt.Errorf("deleted demo image %s remains pinned; no protected image removed", image.target.Digest)
				}
			}
		}

		if len(pending) == 0 {
			return removed, nil
		}

		for config := range pending {
			deleted, err := reclaimImageIdentity(ctx, runtime, config)
			if err != nil {
				return removed, fmt.Errorf("reclaim deleted demo image %s: %w", config, err)
			}

			if deleted {
				removed++

				delete(pending, config)
			}
		}

		if len(pending) == 0 {
			continue
		}

		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return removed, fmt.Errorf("deleted demo images still have runtime references (%d identities); retry cleanup after container removal: %w", len(pending), ctx.Err())
		case <-timer.C:
		}
	}
}

func reclaimImageIdentity(ctx context.Context, runtime reclaimRuntime, config string) (bool, error) {
	removed := false

	// Re-enumerate aliases and protection before every bounded attempt. A
	// successful Delete (or NotFound) alone does not establish image absence.
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		current, err := runtime.listImages(ctx)
		if err != nil {
			return false, fmt.Errorf("refresh image aliases: %w", err)
		}

		containers, err := runtime.listContainerImages(ctx)
		if err != nil {
			return false, fmt.Errorf("protect container images: %w", err)
		}

		var aliases []reclaimImage

		for _, image := range current {
			if image.config == config {
				aliases = append(aliases, image)
			}
		}

		if len(aliases) == 0 {
			return removed, nil
		}

		for _, ref := range containers {
			known := false

			for _, image := range current {
				if reclaimMatchesImage(ref, image) {
					known = true
					break
				}
			}

			// A container whose image metadata no longer resolves may still
			// use any candidate's content. Fail closed, even if stopped.
			if !known {
				return false, fmt.Errorf("cannot verify unused images: container image reference %q does not resolve; no further image records removed", ref)
			}
		}

		for _, alias := range aliases {
			if alias.pinned {
				return false, nil
			}

			for _, ref := range containers {
				if reclaimMatchesImage(ref, alias) {
					return false, nil
				}
			}
		}

		if attempt == 2 {
			return false, fmt.Errorf("image %s still has runtime aliases after 2 removal attempts; concurrent pulls or alias changes may be active", config)
		}

		for _, alias := range aliases {
			if err := runtime.deleteImage(ctx, alias); err != nil && !errdefs.IsNotFound(err) {
				return false, fmt.Errorf("remove image alias %q: %w", alias.name, err)
			}
		}

		removed = true
	}

	return false, errors.New("image absence verification exhausted")
}

func reclaimMatchesImage(ref string, image reclaimImage) bool {
	return ref != "" && (ref == image.config || ref == image.name || ref == image.target.Digest.String() ||
		strings.HasSuffix(ref, "@"+image.target.Digest.String()) || strings.HasSuffix(ref, "@"+image.config))
}

func (r reclaimContainerd) listImages(ctx context.Context) ([]reclaimImage, error) {
	records, err := r.client.ImageService().List(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]reclaimImage, 0, len(records))

	for _, record := range records {
		config, err := record.Config(ctx, r.client.ContentStore(), platforms.DefaultStrict())
		if err != nil {
			// Partial pulls and unknown manifests are not safe candidates.
			return nil, fmt.Errorf("resolve image %q config; image may be pulling: %w", record.Name, err)
		}

		_, pinned := record.Labels["io.cri-containerd.pinned"]

		result = append(result, reclaimImage{
			name: record.Name, target: record.Target, config: config.Digest.String(), pinned: pinned,
		})
	}

	return result, nil
}

func (r reclaimContainerd) listContainerImages(ctx context.Context) ([]string, error) {
	records, err := r.client.ContainerService().List(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]string, 0, len(records))
	for _, record := range records {
		result = append(result, record.Image)
	}

	return result, nil
}

func (r reclaimContainerd) countLeases(ctx context.Context) (int, error) {
	records, err := r.client.LeasesService().List(ctx)
	return len(records), err
}

func (r reclaimContainerd) deleteImage(ctx context.Context, image reclaimImage) error {
	// The target precondition prevents deleting a tag that a concurrent pull
	// retargeted. Synchronous deletion uses containerd's normal reachability GC;
	// existing containers, snapshots, and pull/sandbox leases remain roots.
	return r.client.ImageService().Delete(ctx, image.name, images.DeleteTarget(&image.target), images.SynchronousDelete())
}
