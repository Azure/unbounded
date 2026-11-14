// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerdstore

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	godigest "github.com/opencontainers/go-digest"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

type progressStore struct {
	*fakeStore
	statuses []content.Status
	err      error
}

func (s *progressStore) ListStatuses(context.Context, ...string) ([]content.Status, error) {
	return s.statuses, s.err
}

func TestProgressTracksIngestOffsetsAndCommittedContent(t *testing.T) {
	full := digest.MustParse(godigest.FromString("full").String())
	partial := digest.MustParse(godigest.FromString("partial").String())
	missing := digest.MustParse(godigest.FromString("missing").String())
	fake := &progressStore{fakeStore: newFake(), statuses: []content.Status{
		{Expected: godigest.Digest(partial.String()), Offset: 3, Total: 10},
		{Ref: "gantry-" + partial.String(), Offset: 5, Total: 10},
	}}
	fake.put(godigest.Digest(full.String()), []byte("full"))

	result, err := New(fake).Progress(context.Background(), []digest.Digest{full, partial, missing})
	if err != nil {
		t.Fatal(err)
	}

	if !result[0].Complete || result[0].Available != 4 || result[1].Complete ||
		result[1].Available != 5 || result[2].Available != 0 {
		t.Fatalf("unexpected progress: %+v", result)
	}

	fake.err = errors.New("containerd unavailable")
	if _, err := New(fake).Progress(context.Background(), []digest.Digest{full}); err == nil {
		t.Fatal("unavailable content store must not report zero progress")
	}
}
