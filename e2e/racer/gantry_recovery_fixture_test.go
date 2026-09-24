//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
)

type gantryControl struct {
	mu      sync.Mutex
	configs map[string]*pb.Configuration
	changed chan struct{}
}

// advanceCacheGeneration offers an operator-triggered increment for existing
// volumes on every node. It does not imply activation; use awaitCacheRevision
// before issuing reads that must observe the new namespace.
func (f *gantryFixture) advanceCacheGeneration(volumeIDs ...string) (uint64, error) {
	c := f.controlState
	if c == nil || len(volumeIDs) == 0 {
		return 0, fmt.Errorf("control state and at least one volume are required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	selected := map[string]bool{}
	for _, id := range volumeIDs {
		if selected[id] {
			return 0, fmt.Errorf("duplicate volume %q", id)
		}

		selected[id] = true
	}

	var revision uint64

	configs := make(map[string]*pb.Configuration, len(c.configs))
	for node, current := range c.configs {
		next := proto.Clone(current).(*pb.Configuration)

		s := next.GetSnapshot()
		if s == nil || s.Revision == math.MaxUint64 {
			return 0, fmt.Errorf("node %s has no incrementable snapshot", node)
		}

		s.Revision++
		if revision != 0 && revision != s.Revision {
			return 0, fmt.Errorf("node revisions disagree")
		}

		revision = s.Revision
		found := 0

		for _, v := range s.Volumes {
			if selected[v.Id] {
				// ClusterCache.spec.cacheGeneration is a signed Kubernetes integer.
				if v.CacheGeneration >= math.MaxInt64 {
					return 0, fmt.Errorf("volume %q generation exhausted", v.Id)
				}

				v.CacheGeneration++
				found++
			}
		}

		if found != len(selected) {
			return 0, fmt.Errorf("node %s does not contain every requested volume", node)
		}

		configs[node] = next
	}

	if revision == 0 {
		return 0, fmt.Errorf("no configured nodes")
	}

	c.configs = configs
	close(c.changed)
	c.changed = make(chan struct{})

	return revision, nil
}

// bumpCacheGeneration returns only after all real dataplanes activate the bump.
func (f *gantryFixture) bumpCacheGeneration(volumeIDs ...string) uint64 {
	f.t.Helper()

	revision, err := f.advanceCacheGeneration(volumeIDs...)
	if err != nil {
		f.t.Fatal(err)
	}

	f.awaitCacheRevision(revision)

	return revision
}

func (f *gantryFixture) awaitCacheRevision(revision uint64) {
	f.t.Helper()

	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Second)
	defer cancel()

	if err := f.waitCacheRevision(ctx, revision); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gantryFixture) waitCacheRevision(ctx context.Context, revision uint64) error {
	if f.controlState == nil || revision == 0 {
		return fmt.Errorf("control state and a nonzero revision are required")
	}

	f.controlState.mu.Lock()
	nodes := len(f.controlState.configs)
	f.controlState.mu.Unlock()

	if nodes == 0 || len(f.racerMetrics) != nodes {
		return fmt.Errorf("need status endpoints for all %d nodes, have %d", nodes, len(f.racerMetrics))
	}

	var last error
	for {
		last = nil

		for _, address := range f.racerMetrics {
			if err := f.cacheRevisionStatus(ctx, address, revision); err != nil {
				last = err
				break
			}
		}

		if last == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for active cache revision %d: %w: %v", revision, ctx.Err(), last)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (f *gantryFixture) cacheRevisionStatus(ctx context.Context, address string, revision uint64) error {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/status", nil)
	if err != nil {
		return err
	}

	resp, err := f.client.Do(r)
	if err != nil {
		return fmt.Errorf("%s: %w", address, err)
	}
	defer resp.Body.Close()

	var status struct {
		ActiveRevision    uint64 `json:"activeRevision"`
		CandidateRevision uint64 `json:"candidateRevision"`
		LocalState        string `json:"localState"`
		Ready             bool   `json:"ready"`
		Rejected          bool   `json:"rejected"`
		Workers           int    `json:"workers"`
		ActivatedWorkers  int    `json:"activatedWorkers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("%s: status HTTP %d: %w", address, resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK || !status.Ready || status.Rejected || status.ActiveRevision != revision || status.CandidateRevision != revision || status.LocalState != "applied" || status.Workers == 0 || status.ActivatedWorkers != status.Workers {
		return fmt.Errorf("%s: status HTTP %d: %+v", address, resp.StatusCode, status)
	}

	return nil
}

func cloneGantryObjects(objects map[string]gantryObject) map[string]gantryObject {
	result := make(map[string]gantryObject, len(objects))
	for path, obj := range objects {
		obj.data = bytes.Clone(obj.data)
		result[path] = obj
	}

	return result
}

// setOriginObject atomically replaces an existing origin object. In-flight
// requests retain their old immutable bytes; subsequent requests see the repair.
// The caller's slice is copied and the URL (including its digest) is unchanged.
func (f *gantryFixture) setOriginObject(path string, obj gantryObject) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.objects[path]; !ok {
		return fmt.Errorf("unknown origin object %q", path)
	}

	obj.data = bytes.Clone(obj.data)
	f.objects[path] = obj

	return nil
}

// originRequestCount counts attempts, including failed/offline requests, across
// all origins. Empty method or path matches all values of that field.
func (f *gantryFixture) originRequestCount(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0

	for _, hit := range f.hits {
		if (method == "" || hit.method == method) && (path == "" || hit.path == path) {
			count++
		}
	}

	return count
}
