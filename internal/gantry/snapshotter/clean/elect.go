// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clean

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Always elects this node for every segment. It is what a single node cluster
// wants, and what any cluster gets when no membership view is configured.
type Always struct{}

// Elected always reports true.
func (Always) Elected(uint32) bool { return true }

// HRW elects one node per segment by rendezvous hashing.
//
// Cleaning a segment is a read and a write of every survivor in it, so having
// every node do it would multiply the cost by the size of the cluster for no
// benefit. Rendezvous rather than a lock service because the catalog's
// compare-and-swaps already make a race harmless: the election is an efficiency
// measure, and it degrades to Always when the membership view is empty.
type HRW struct {
	// Self is this node.
	Self ifaces.NodeID

	// Members returns the current membership view.
	Members func() []ifaces.Node
}

// Elected reports whether this node ranks first for the segment.
func (h HRW) Elected(id uint32) bool {
	if h.Members == nil {
		return true
	}

	members := h.Members()
	if len(members) == 0 {
		return true
	}

	candidates := hrw.Candidates(members, hrw.ScopeCluster, "")
	if len(candidates) == 0 {
		return true
	}

	return hrw.RankOf(candidates, h.Self, segmentKey(id)) == 0
}

// segmentKey turns a segment id into something the rendezvous hash can weigh.
//
// The id is a small integer and the hash mixes the key with the node id, so a
// raw counter would place segments in a pattern that repeats across the
// cluster. Hashing it first spreads them.
func segmentKey(id uint32) digest.Digest {
	var buf [4]byte

	binary.LittleEndian.PutUint32(buf[:], id)

	sum := sha256.Sum256(buf[:])

	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}
