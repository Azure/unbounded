// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// walk.go contains the platform-agnostic helpers that traverse
// containerd's content store. They have no Linux-only dependencies and
// live outside source_containerd.go so they can be unit-tested on
// darwin against an in-memory fake content store.

package cdsub

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"
)

// walkBlobs traverses target's manifest tree in the supplied content
// store and returns every blob digest that is **present and serveable**
// from containerd right now in DFS order: the target descriptor itself,
// its config (for image manifests), its layers, plus every child of an
// image index.
//
// Uses images.Walk + images.Children which are exactly the helpers
// containerd internally uses for image GC. Each visited descriptor is
// probed with ContentStore.Info — a single map lookup — so the
// resulting digest set is guaranteed to be serveable from the local
// content store at probe time. This enforces the plan invariant
// "advertise only present-and-serveable from containerd" instead of
// the prior "referenced by manifest" behaviour, which produced phantom
// DHT provider records the transfer endpoint then 404'd on.
//
// Tolerates errdefs.IsNotFound errors from both images.Children and
// ContentStore.Info: under CRI, kubelet pulls only the platform-
// relevant subtree of a multi-arch image index, so attestation
// manifests and other-arch child manifests are referenced by the
// index but absent from the content store. Absent subtrees are
// skipped silently so the rest of the image still produces useful
// announcements.
func walkBlobs(ctx context.Context, store content.Store, target ocispec.Descriptor) ([]gdigest.Digest, error) {
	return walkBlobsWithRecorder(ctx, store, target, nil)
}

// walkBlobsWithRecorder is walkBlobs that additionally calls recorder
// for every present descriptor's (digest, mediaType) pair. Used by
// the containerdstore descriptor index — populating it during the
// reconcile-walk we already do (per plan §"Descriptor index"
// sources of truth) means the transfer endpoint's manifest replies
// can fill the Content-Type header without parsing the manifest
// body. recorder may be nil; nil is a no-op for back-compat with
// callers that only want the digest set.
func walkBlobsWithRecorder(ctx context.Context, store content.Store, target ocispec.Descriptor, recorder func(d gdigest.Digest, mediaType string)) ([]gdigest.Digest, error) {
	var (
		out  []gdigest.Digest
		seen = map[string]struct{}{}
	)
	handler := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if desc.Digest == "" {
			return nil, nil
		}
		s := desc.Digest.String()
		if _, ok := seen[s]; ok {
			return nil, nil
		}
		seen[s] = struct{}{}
		d, parseErr := gdigest.Parse(s)
		if parseErr != nil {
			// Skip non-sha256 entries; the rest of the agent only
			// handles sha256 (per internal/digest).
			return childrenIfPresent(ctx, store, desc)
		}
		// Present-only gate: only emit the digest if the content store
		// can confirm it is locally available. Info() is metadata-only
		// and cheap. Absent digests are skipped silently so a missing
		// child does not invalidate sibling subtrees.
		if _, infoErr := store.Info(ctx, desc.Digest); infoErr != nil {
			if errdefs.IsNotFound(infoErr) {
				return childrenIfPresent(ctx, store, desc)
			}
			return nil, infoErr
		}
		if recorder != nil && desc.MediaType != "" {
			recorder(d, desc.MediaType)
		}
		out = append(out, d)
		return childrenIfPresent(ctx, store, desc)
	})
	if err := images.Walk(ctx, handler, target); err != nil {
		return nil, err
	}
	return out, nil
}

// childrenIfPresent calls images.Children and downgrades a content-
// store "not found" miss to (nil, nil). See walkBlobs for the rationale.
func childrenIfPresent(ctx context.Context, store content.Store, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	children, err := images.Children(ctx, store, desc)
	if err != nil && errdefs.IsNotFound(err) {
		return nil, nil
	}
	return children, err
}
