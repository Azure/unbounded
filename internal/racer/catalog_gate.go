// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "context"

// CatalogGate serializes authoritative catalog and credential operations while
// allowing callers to abandon admission when their context is canceled.
type CatalogGate struct {
	token chan struct{}
}

func newCatalogGate() *CatalogGate {
	g := &CatalogGate{token: make(chan struct{}, 1)}
	g.token <- struct{}{}

	return g
}

// Acquire returns ownership only for a live context. A failed acquisition must
// not be released and does not constitute an observation of invalid authority.
func (g *CatalogGate) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
		if err := ctx.Err(); err != nil {
			g.Release()
			return err
		}

		return nil
	}
}

// Release ends a successfully acquired critical section.
func (g *CatalogGate) Release() {
	g.token <- struct{}{}
}
