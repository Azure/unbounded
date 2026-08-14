// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// Blob is a resolved layer: where its EROFS image lives and what it hashes to.
type Blob struct {
	// DiffID is the layer's uncompressed digest, which is what a blob is
	// keyed by. Two images sharing a layer share this, and therefore share
	// the blob.
	DiffID Digest

	// Address is the blob's location in a segment.
	Address segment.Address

	// Sum is the sha256 of the blob bytes, for offline scrub. It is not
	// verified at mount time: doing so would read the whole blob and defeat
	// demand paging, which is the entire point.
	Sum Digest

	// Generation is the catalog generation the blob was last published at.
	// A blob moved by the cleaner reappears at a higher generation, and a
	// node reports the lowest generation any of its live mounts depends on
	// so the cleaner knows when a drain is complete.
	Generation uint64
}

// Index is the resolved, in-memory view of the catalog's records.
//
// It is pure: it holds no device and does no I/O. Store owns an Index and
// feeds it records as they are read. That split is what lets the resolution
// rules be tested exhaustively without a block device.
type Index struct {
	// entries is keyed by a record's Key, which is a diffID for blobs and a
	// chainID for chains. The two spaces do not collide in practice and,
	// more to the point, a collision would be a sha256 collision.
	entries map[Digest]entry
}

type entry struct {
	generation uint64
	kind       RecordType
	record     Record
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{entries: make(map[Digest]entry)}
}

// Apply folds records into the index.
//
// Records for the same key are ordered by generation and the highest wins.
// Append order already is generation order, because generations come from the
// superblock and strictly increase, but ordering explicitly means a reader
// that catches up out of order, or replays, still converges on the same
// answer.
func (i *Index) Apply(records ...Record) {
	for _, r := range records {
		if r.Type == RecordUnused {
			continue
		}

		// A void is a retired slot, not a statement about a key. It
		// carries no key at all, so folding it in would create an entry
		// under the zero digest and let a later void retire it again.
		if r.Type == RecordVoid {
			continue
		}

		if prior, ok := i.entries[r.Key]; ok && prior.generation >= r.Generation {
			continue
		}

		if r.Type == RecordTombstone {
			// A tombstone is kept as a marker rather than applied as a
			// delete. Deleting would leave no trace of the retirement,
			// and the next older record for the same key - a replay, a
			// reader catching up out of order, or a second pass over
			// the log - would find no prior entry to lose to and would
			// resurrect a blob whose pages may already have been
			// trimmed. Keeping it costs one map entry and makes the
			// ordering rule above true for every record type.
			i.entries[r.Key] = entry{generation: r.Generation, kind: r.Type, record: r}

			continue
		}

		i.entries[r.Key] = entry{generation: r.Generation, kind: r.Type, record: r}
	}
}

// Blob resolves a layer's diffID to its blob.
func (i *Index) Blob(diffID Digest) (Blob, bool) {
	e, ok := i.entries[diffID]
	if !ok || e.kind != RecordBlob {
		return Blob{}, false
	}

	return Blob{
		DiffID:     e.record.Key,
		Address:    e.record.Address(),
		Sum:        e.record.Ref,
		Generation: e.record.Generation,
	}, true
}

// Resolve maps a containerd chainID to the blob that serves it.
//
// This is the lookup on the container start critical path. A hit is what lets
// Prepare return ErrAlreadyExists, which is what makes containerd skip both
// the download and the unpack of that layer.
//
// The indirection through a chain record is deliberate: two images whose
// layer stacks diverge higher up still share the blob for a common layer, and
// a blob moved by the cleaner is repointed in one place rather than once per
// image that references it.
func (i *Index) Resolve(chainID Digest) (Blob, bool) {
	e, ok := i.entries[chainID]
	if !ok || e.kind != RecordChain {
		return Blob{}, false
	}

	blob, ok := i.Blob(e.record.Ref)
	if !ok {
		// A chain record whose blob is gone. This is reachable: the blob's
		// tombstone landed and the chain's did not, or has not been read
		// yet. Report a miss and take the local unpack path, which is
		// correct, just slower.
		return Blob{}, false
	}

	// The chain record's generation, not the blob's, is what this mount
	// depends on for drain accounting: the chain is the edge that could be
	// repointed out from under us.
	if e.record.Generation > blob.Generation {
		blob.Generation = e.record.Generation
	}

	return blob, true
}

// BlobsIn returns every blob the index currently resolves into a segment.
//
// This is the cleaner's work list: the survivors it has to copy out before the
// segment can be trimmed. It reads the in-memory index rather than the device
// because every node already holds the whole record set, and because what
// matters is which blobs are live now, not which records were ever written. A
// blob that has already been copied elsewhere resolves to its new home and is
// not in this segment's list any more, so a resumed cycle does not copy it
// twice.
func (i *Index) BlobsIn(segment uint32) []Blob {
	var blobs []Blob

	for _, e := range i.entries {
		if e.kind != RecordBlob || e.record.Segment != segment {
			continue
		}

		blobs = append(blobs, Blob{
			DiffID:     e.record.Key,
			Address:    e.record.Address(),
			Sum:        e.record.Ref,
			Generation: e.record.Generation,
		})
	}

	// Map iteration is random and the cleaner reports progress by blob, so an
	// order that changed between passes would make a resumed cycle look like
	// a different one.
	sort.Slice(blobs, func(a, b int) bool {
		return bytes.Compare(blobs[a].DiffID[:], blobs[b].DiffID[:]) < 0
	})

	return blobs
}

// Len is how many keys the index resolves.
func (i *Index) Len() int { return len(i.entries) }

// ParseDigest decodes a containerd digest string of the form "sha256:<hex>".
//
// Only sha256 is representable in a record's fixed 32 bytes. A layer with any
// other algorithm is never ingested and always takes the local unpack path,
// which is why this reports a plain error rather than something the caller has
// to distinguish.
func ParseDigest(s string) (Digest, error) {
	const prefix = "sha256:"

	var d Digest

	if len(s) != len(prefix)+2*DigestBytes || s[:len(prefix)] != prefix {
		return d, fmt.Errorf("digest %q is not a sha256 digest", s)
	}

	if _, err := hex.Decode(d[:], []byte(s[len(prefix):])); err != nil {
		return d, fmt.Errorf("digest %q: %w", s, err)
	}

	return d, nil
}

// String renders a digest the way containerd writes it.
func (d Digest) String() string { return "sha256:" + hex.EncodeToString(d[:]) }

// Short renders the first 12 hex characters of a digest, which is what mount
// point paths use. Overlayfs caps its mount options at a page, and 40 layers
// of full-length digests would not fit.
func (d Digest) Short() string { return hex.EncodeToString(d[:6]) }
