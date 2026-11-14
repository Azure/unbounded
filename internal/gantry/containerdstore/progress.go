// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerdstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

// BlobProgress reports local content, not cumulative transfer traffic.
type BlobProgress struct {
	Digest    string `json:"digest"`
	Available int64  `json:"available"`
	Total     int64  `json:"total"`
	Complete  bool   `json:"complete"`
}

func (s *Store) Progress(ctx context.Context, digests []digest.Digest) ([]BlobProgress, error) {
	ctx = s.withNS(ctx)

	statuses, err := s.cs.ListStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("list content ingests: %w", err)
	}

	result := make([]BlobProgress, 0, len(digests))
	for _, d := range digests {
		blob := BlobProgress{Digest: d.String()}

		reader, err := s.cs.ReaderAt(ctx, ocispec.Descriptor{Digest: godigest.Digest(d.String())})
		switch {
		case err == nil:
			blob.Available, blob.Total, blob.Complete = reader.Size(), reader.Size(), true
			if err := reader.Close(); err != nil {
				return nil, fmt.Errorf("close content reader: %w", err)
			}
		case errors.Is(err, cerrdefs.ErrNotFound):
			for _, status := range statuses {
				// Some containerd ingests omit Expected but retain the digest in
				// their reference, including Gantry's "gantry-" references.
				matches := status.Expected == godigest.Digest(d.String()) ||
					(status.Expected == "" && strings.HasSuffix(status.Ref, d.String()))
				if matches && status.Offset > blob.Available {
					blob.Available, blob.Total = status.Offset, status.Total
				}
			}
		default:
			return nil, fmt.Errorf("read content progress: %w", err)
		}

		result = append(result, blob)
	}

	return result, nil
}
