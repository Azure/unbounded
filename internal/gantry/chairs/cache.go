// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chairs

import (
	"context"
	"fmt"
	"sync"
)

type SnapshotReader interface {
	Snapshot(ctx context.Context, epoch int64) (Snapshot, error)
	Get(ctx context.Context, id ID) (Chair, error)
}

type Cache struct {
	reader SnapshotReader

	mu           sync.Mutex
	snapshot     Snapshot
	refresh      *snapshotRefresh
	invalid      bool
	chairRefresh map[ID]*chairRefresh
}

type snapshotRefresh struct {
	done     chan struct{}
	snapshot Snapshot
	err      error
}

type chairRefresh struct {
	done  chan struct{}
	chair Chair
	err   error
}

func NewCache(reader SnapshotReader) *Cache {
	return &Cache{reader: reader, chairRefresh: make(map[ID]*chairRefresh)}
}

func (c *Cache) Snapshot(ctx context.Context, epoch int64) (Snapshot, error) {
	c.mu.Lock()
	if !c.invalid && c.snapshot.Epoch == epoch && c.snapshot.SelectableCount() >= SeedCount {
		snapshot := cloneSnapshot(c.snapshot)
		c.mu.Unlock()

		return snapshot, nil
	}

	if c.refresh != nil {
		refresh := c.refresh
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-refresh.done:
		}

		if refresh.err != nil && refresh.snapshot.SelectableCount() < SeedCount {
			return Snapshot{}, fmt.Errorf("refresh chair snapshot: %w", refresh.err)
		}

		return cloneSnapshot(refresh.snapshot), nil
	}

	refresh := &snapshotRefresh{done: make(chan struct{})}
	c.refresh = refresh
	c.mu.Unlock()

	snapshot, err := c.reader.Snapshot(ctx, epoch)

	c.mu.Lock()
	if err == nil {
		c.snapshot = cloneSnapshot(snapshot)
		c.invalid = false
	} else if c.snapshot.SelectableCount() >= SeedCount {
		snapshot = cloneSnapshot(c.snapshot)
		snapshot.Stale = true
	} else {
		snapshot = Snapshot{}
	}

	refresh.snapshot = cloneSnapshot(snapshot)
	refresh.err = err
	close(refresh.done)
	c.refresh = nil
	c.mu.Unlock()

	if err != nil && snapshot.SelectableCount() < SeedCount {
		return Snapshot{}, fmt.Errorf("refresh chair snapshot: %w", err)
	}

	return snapshot, nil
}

func (c *Cache) RefreshChair(ctx context.Context, id ID) (Chair, error) {
	c.mu.Lock()
	if refresh := c.chairRefresh[id]; refresh != nil {
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return Chair{}, ctx.Err()
		case <-refresh.done:
			return cloneChair(refresh.chair), refresh.err
		}
	}

	refresh := &chairRefresh{done: make(chan struct{})}
	c.chairRefresh[id] = refresh
	c.mu.Unlock()

	chair, err := c.reader.Get(ctx, id)
	if err != nil {
		c.mu.Lock()
		refresh.err = err
		close(refresh.done)
		delete(c.chairRefresh, id)
		c.mu.Unlock()

		return Chair{}, err
	}

	c.mu.Lock()
	replaced := false

	for index := range c.snapshot.Chairs {
		if c.snapshot.Chairs[index].ID == id {
			c.snapshot.Chairs[index] = chair
			replaced = true

			break
		}
	}

	if !replaced {
		c.snapshot.Chairs = append(c.snapshot.Chairs, chair)
	}

	refresh.chair = cloneChair(chair)
	close(refresh.done)
	delete(c.chairRefresh, id)
	c.mu.Unlock()

	return chair, nil
}

func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.invalid = true
	c.mu.Unlock()
}

func (c *Cache) Peek() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	return cloneSnapshot(c.snapshot)
}

func (c *Cache) Observe(snapshot Snapshot) {
	c.mu.Lock()
	c.snapshot = cloneSnapshot(snapshot)
	c.invalid = false
	c.mu.Unlock()
}

func (c *Cache) UpdateChair(chair Chair) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for index := range c.snapshot.Chairs {
		if c.snapshot.Chairs[index].ID == chair.ID {
			c.snapshot.Chairs[index] = chair
			return
		}
	}

	c.snapshot.Chairs = append(c.snapshot.Chairs, chair)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	out := snapshot

	out.Chairs = append([]Chair(nil), snapshot.Chairs...)
	for index := range out.Chairs {
		out.Chairs[index].Holder.P2PAddrs = append([]string(nil), out.Chairs[index].Holder.P2PAddrs...)
		out.Chairs[index].NextHolder.P2PAddrs = append([]string(nil), out.Chairs[index].NextHolder.P2PAddrs...)
	}

	return out
}

func cloneChair(chair Chair) Chair {
	chair.Holder.P2PAddrs = append([]string(nil), chair.Holder.P2PAddrs...)
	chair.NextHolder.P2PAddrs = append([]string(nil), chair.NextHolder.P2PAddrs...)

	return chair
}
