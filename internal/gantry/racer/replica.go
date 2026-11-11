// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
)

const (
	// replicaCacheEntries bounds remembered verification results. Completed
	// entries beyond it are evicted oldest first, so an evicted file is simply
	// hashed again on its next local-only request.
	replicaCacheEntries = 4096
	// replicaHashWorkers bounds concurrent full-file hashes so verification
	// cannot saturate the disk that also serves pages.
	replicaHashWorkers = 2
	// replicaPendingLimit bounds files waiting for or undergoing a hash. A
	// request beyond it is a local miss and Racer falls through to peers or
	// the origin.
	replicaPendingLimit = 256
)

// Replica verification outcomes, used as metric label values.
const (
	ReplicaVerified = "verified"
	ReplicaCorrupt  = "corrupt"
	ReplicaError    = "error"
)

var (
	errReplicaCorrupt     = errors.New("local replica failed digest verification")
	errReplicaUnverified  = errors.New("local replica not yet verified")
	errReplicaBusy        = errors.New("local replica verification queue full")
	errReplicaUnverifying = errors.New("local replica changed during verification")
)

type replicaState int

const (
	replicaPending replicaState = iota
	replicaGood
	replicaBad
)

// replicaID identifies one file version. A replaced or rewritten file gets a
// new identity (inode, size, or timestamps), which discards earlier results.
type replicaID struct {
	digest       string
	dev, ino     uint64
	size         int64
	mtime, ctime int64
}

type replicaEntry struct {
	state replicaState
	done  chan struct{}
}

// ReplicaVerifier hashes each local replica once before it is served to
// Racer, and quarantines replicas whose bytes do not match their digest until
// the file changes. Containerd verifies digests on commit, so a mismatch means
// on-disk corruption or tampering. Without this check, Invalidate would drop
// Racer's state but the next local-only request would reopen the same bad
// file, on this node or on a remote owner, and recovery could never reach the
// registry. Every node verifies its own files, which covers remote owners.
type ReplicaVerifier struct {
	logger   *slog.Logger
	observe  func(result string)
	sem      chan struct{}
	mu       sync.Mutex
	entries  map[replicaID]*replicaEntry
	order    []replicaID // completion order, for FIFO eviction
	pending  int
	maxCache int
}

// NewReplicaVerifier returns a verifier. observe, if non-nil, receives each
// completed verification's result and must be concurrency-safe.
func NewReplicaVerifier(logger *slog.Logger, observe func(result string)) *ReplicaVerifier {
	if logger == nil {
		logger = slog.Default()
	}

	return &ReplicaVerifier{
		logger: logger, observe: observe,
		sem:      make(chan struct{}, replicaHashWorkers),
		entries:  make(map[replicaID]*replicaEntry),
		maxCache: replicaCacheEntries,
	}
}

// Forget drops results for digest so its local replica is hashed again. The
// mirror calls it when a response for digest fails verification.
func (v *ReplicaVerifier) Forget(digest string) {
	if v == nil {
		return
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	for id, e := range v.entries {
		if id.digest == digest && e.state != replicaPending {
			delete(v.entries, id)
		}
	}
}

// check reports nil when blob, opened from store under digest, is verified.
// An unverified file starts a background hash that outlives ctx, so a later
// request benefits even if this one gives up. The returned error is a local
// miss reason.
func (v *ReplicaVerifier) check(ctx context.Context, store LocalStore, digest string, blob LocalBlob, size int64) error {
	id, ok := blobID(digest, blob, size)
	if !ok {
		// No stable identity: verify inline and remember nothing.
		return v.hashInline(ctx, digest, blob, size)
	}

	v.mu.Lock()

	e, found := v.entries[id]
	if !found {
		if v.pending >= replicaPendingLimit {
			v.mu.Unlock()

			return errReplicaBusy
		}

		e = &replicaEntry{done: make(chan struct{})}
		v.entries[id] = e
		v.pending++

		go v.verify(store, id, e)
	}
	v.mu.Unlock()

	select {
	case <-e.done:
	case <-ctx.Done():
		return errors.Join(errReplicaUnverified, ctx.Err())
	}

	v.mu.Lock()
	state := e.state
	v.mu.Unlock()

	switch state {
	case replicaGood:
		return nil
	case replicaBad:
		return errReplicaCorrupt
	default:
		return errReplicaUnverifying
	}
}

// verify hashes a freshly opened copy of the file so the requester keeps
// sole ownership of its handle and may give up without stopping the hash.
func (v *ReplicaVerifier) verify(store LocalStore, id replicaID, e *replicaEntry) {
	state, result := replicaPending, ReplicaError

	defer func() {
		v.finish(id, e, state)

		if v.observe != nil {
			v.observe(result)
		}
	}()

	v.sem <- struct{}{}
	defer func() { <-v.sem }()

	blob, size, err := store.Open(id.digest)
	if err != nil {
		v.logger.Warn("Racer local replica verification could not open blob", "digest", id.digest, "error", err)

		return
	}
	defer blob.Close() //nolint:errcheck // read-only handle

	if got, ok := blobID(id.digest, blob, size); !ok || got != id {
		// The file changed between the request's open and ours; leave the
		// identity unverified so the next request starts over.
		return
	}

	good, err := hashMatches(context.Background(), id.digest, blob, size)
	if err != nil {
		v.logger.Warn("Racer local replica verification read failed", "digest", id.digest, "error", err)

		return
	}

	if good {
		state, result = replicaGood, ReplicaVerified

		return
	}

	state, result = replicaBad, ReplicaCorrupt

	v.logger.Error("Racer local replica failed digest verification; quarantined until the file changes",
		"digest", id.digest, "size", size)
}

func (v *ReplicaVerifier) finish(id replicaID, e *replicaEntry, state replicaState) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.pending--
	e.state = state

	close(e.done)

	if state == replicaPending {
		// Errors are not cached; the next request retries.
		if v.entries[id] == e {
			delete(v.entries, id)
		}

		return
	}

	v.order = append(v.order, id)
	for len(v.order) > 0 && len(v.entries) > v.maxCache {
		old := v.order[0]
		v.order = v.order[1:]

		if old2, ok := v.entries[old]; ok && old2.state != replicaPending {
			delete(v.entries, old)
		}
	}

	// Compact the order slice when stale ids (forgotten entries) pile up.
	if len(v.order) > 2*v.maxCache {
		kept := v.order[:0]
		for _, o := range v.order {
			if x, ok := v.entries[o]; ok && x.state != replicaPending {
				kept = append(kept, o)
			}
		}

		v.order = kept
	}
}

func (v *ReplicaVerifier) hashInline(ctx context.Context, digest string, blob LocalBlob, size int64) error {
	good, err := hashMatches(ctx, digest, blob, size)
	if err != nil {
		return err
	}

	if !good {
		if v.observe != nil {
			v.observe(ReplicaCorrupt)
		}

		return errReplicaCorrupt
	}

	return nil
}

// hashMatches reports whether blob's first size bytes hash to digest.
func hashMatches(ctx context.Context, digest string, blob io.ReaderAt, size int64) (bool, error) {
	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx, io.NewSectionReader(blob, 0, size)}); err != nil {
		return false, err
	}

	return "sha256:"+hex.EncodeToString(h.Sum(nil)) == digest, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}

// blobID derives a file identity from fstat of the open handle.
func blobID(digest string, blob LocalBlob, size int64) (replicaID, bool) {
	s, ok := blob.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return replicaID{}, false
	}

	info, err := s.Stat()
	if err != nil || info.Size() != size {
		return replicaID{}, false
	}

	id := replicaID{digest: digest, size: size, mtime: info.ModTime().UnixNano()}
	id.dev, id.ino, id.ctime = fileSys(info)

	return id, true
}
