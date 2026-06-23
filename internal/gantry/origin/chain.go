// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package origin

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// PriorityChain tries each cache entry in order before falling back to
// the mandatory fallback (typically the OCI registry client). A cache
// entry that returns any *ifaces.OriginError is skipped with a WARN log;
// the fallback error is propagated as-is.
type PriorityChain struct {
	entries  []ifaces.OriginPuller
	fallback ifaces.OriginPuller
	log      *slog.Logger
}

// NewPriorityChain builds a chain. entries may be nil or empty (chain
// degenerates to the fallback). fallback must not be nil.
func NewPriorityChain(entries []ifaces.OriginPuller, fallback ifaces.OriginPuller, log *slog.Logger) *PriorityChain {
	return &PriorityChain{
		entries:  entries,
		fallback: fallback,
		log:      log,
	}
}

// Pull implements ifaces.OriginPuller. It tries cache entries first,
// then falls back.
func (c *PriorityChain) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	for _, entry := range c.entries {
		rc, size, err := entry.Pull(ctx, ref)
		if err == nil {
			return rc, size, nil
		}

		var oe *ifaces.OriginError
		if errors.As(err, &oe) {
			c.log.Warn("cache origin miss, trying next",
				"digest", ref.Digest.String(),
				"class", oe.Class,
			)

			continue
		}

		c.log.Warn("cache origin unexpected error, trying next",
			"digest", ref.Digest.String(),
			"err", err,
		)
	}

	return c.fallback.Pull(ctx, ref)
}

// Head implements ifaces.OriginPuller. It tries cache entries first,
// then falls back.
func (c *PriorityChain) Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error) {
	for _, entry := range c.entries {
		size, ct, err := entry.Head(ctx, ref)
		if err == nil {
			return size, ct, nil
		}

		var oe *ifaces.OriginError
		if errors.As(err, &oe) {
			c.log.Warn("cache origin miss, trying next",
				"digest", ref.Digest.String(),
				"class", oe.Class,
			)

			continue
		}

		c.log.Warn("cache origin unexpected error, trying next",
			"digest", ref.Digest.String(),
			"err", err,
		)
	}

	return c.fallback.Head(ctx, ref)
}
