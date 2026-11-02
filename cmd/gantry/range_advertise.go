package main

import (
	"bytes"
	"context"
	"io"

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

type rangeAwareInventory struct {
	inv   advertise.Inventory
	open  advertise.Openable
	cache *rangecache.Cache
}

func (r rangeAwareInventory) Inventory(ctx context.Context) ([]digest.Digest, error) {
	base, err := r.inv.Inventory(ctx)
	if err != nil {
		return nil, err
	}

	if r.cache == nil {
		return base, nil
	}

	seen := make(map[digest.Digest]struct{}, len(base))
	for _, d := range base {
		seen[d] = struct{}{}
	}

	for _, d := range r.cache.Digests() {
		if _, ok := seen[d]; ok {
			continue
		}

		base = append(base, d)
	}

	return base, nil
}

func (r rangeAwareInventory) Open(ctx context.Context, d digest.Digest) (io.ReadCloser, int64, error) {
	if r.open != nil {
		body, size, err := r.open.Open(ctx, d)
		if err == nil {
			return body, size, nil
		}
	}

	if r.cache != nil && r.cache.HasAny(d) {
		return io.NopCloser(bytes.NewReader(nil)), 0, nil
	}

	return nil, 0, &ifaces.ErrNotFound{Digest: d}
}
