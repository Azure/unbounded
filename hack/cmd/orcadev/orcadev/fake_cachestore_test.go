// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"context"
	"fmt"
)

// fakeCachestore implements cachestoreOps in-memory so scenario
// tests can drive the full cold-warm step sequence (and its failure
// paths) without standing up a real S3 endpoint.
//
// When matchAll is true Head succeeds for every probed path,
// regardless of the objects map; useful for forcing the clear loop
// to enter the Delete branch when the test cannot predict the
// scenario-generated chunk key in advance.
type fakeCachestore struct {
	objects   map[string]cacheObject
	headErr   error
	deleteErr error
	matchAll  bool
}

func newFakeCachestore() *fakeCachestore {
	return &fakeCachestore{objects: map[string]cacheObject{}}
}

func (f *fakeCachestore) Head(_ context.Context, path string) (cacheObject, error) {
	if f.headErr != nil {
		return cacheObject{}, f.headErr
	}

	if f.matchAll {
		return cacheObject{Path: path, Size: 1}, nil
	}

	obj, ok := f.objects[path]
	if !ok {
		return cacheObject{}, ErrCacheNotFound
	}

	return obj, nil
}

func (f *fakeCachestore) Delete(_ context.Context, path string) error {
	if f.deleteErr != nil {
		return fmt.Errorf("fake cachestore: %w", f.deleteErr)
	}

	delete(f.objects, path)

	return nil
}

var _ cachestoreOps = (*fakeCachestore)(nil)
