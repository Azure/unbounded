// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chairs models Gantry's fixed Kubernetes Lease chairs.
package chairs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	Count      = 64
	SeedCount  = 8
	NamePrefix = "gantry-chair-"
)

type ID uint8

func (id ID) Name() string {
	return fmt.Sprintf("%s%02d", NamePrefix, id)
}

func ParseName(name string) (ID, error) {
	if !strings.HasPrefix(name, NamePrefix) {
		return 0, fmt.Errorf("chair name %q does not start with %q", name, NamePrefix)
	}

	raw := strings.TrimPrefix(name, NamePrefix)

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n >= Count {
		return 0, fmt.Errorf("invalid chair name %q", name)
	}

	return ID(n), nil
}

type Holder = ifaces.PeerEndpoint

func HolderEmpty(h Holder) bool {
	return h.PeerID == ""
}

type Chair struct {
	ID              ID
	Holder          Holder
	Generation      int64
	AssignmentEpoch int64
	RenewTime       time.Time
	LeaseDuration   time.Duration
	NextHolder      Holder
}

func (c Chair) Occupied() bool {
	return !HolderEmpty(c.Holder)
}

func (c Chair) Selectable() bool {
	return c.Occupied() && len(c.Holder.P2PAddrs) > 0 && c.Holder.TransferAddr != ""
}

func (c Chair) Expired(now time.Time) bool {
	return c.Occupied() && c.LeaseDuration > 0 && !now.Before(c.RenewTime.Add(c.LeaseDuration))
}

type Snapshot struct {
	Epoch     int64
	Chairs    []Chair
	FetchedAt time.Time
	Stale     bool
}

func (s Snapshot) OccupiedCount() int {
	count := 0

	for _, chair := range s.Chairs {
		if chair.Occupied() {
			count++
		}
	}

	return count
}

func (s Snapshot) AvailableCount() int {
	count := 0

	for _, chair := range s.Chairs {
		if chair.Selectable() && chair.AssignmentEpoch == s.Epoch {
			count++
		}
	}

	return count
}

// SelectableCount includes current holders plus holders from the immediately
// preceding epoch. Epoch transitions are distributed Lease updates, so the
// previous holder remains the valid dial target until its Lease is renewed or
// rotated. Older snapshots returned during an API outage carry their original
// Snapshot.Epoch and remain usable as dial hints.
func (s Snapshot) SelectableCount() int {
	count := 0

	for _, chair := range s.Chairs {
		if chair.Selectable() && (chair.AssignmentEpoch == s.Epoch || chair.AssignmentEpoch == s.Epoch-1) {
			count++
		}
	}

	return count
}

func (s Snapshot) HolderChair(peerID ifaces.NodeID) (Chair, bool) {
	for _, chair := range s.Chairs {
		if chair.Holder.PeerID == peerID {
			return chair, true
		}
	}

	return Chair{}, false
}

func Rank(snapshot Snapshot, d digest.Digest) []Chair {
	byName := make(map[string]Chair, len(snapshot.Chairs))
	candidates := make([]ifaces.Node, 0, Count)

	for index := range Count {
		id := ID(index)
		name := id.Name()
		candidates = append(candidates, ifaces.Node{ID: ifaces.NodeID(name)})
	}

	for _, chair := range snapshot.Chairs {
		if chair.AssignmentEpoch != snapshot.Epoch && chair.AssignmentEpoch != snapshot.Epoch-1 {
			continue
		}

		byName[chair.ID.Name()] = chair
	}

	scored := hrw.TopK(candidates, d, Count)
	ranked := make([]Chair, 0, snapshot.OccupiedCount())

	for _, candidate := range scored {
		chair, ok := byName[string(candidate.Node.ID)]
		if !ok || !chair.Selectable() {
			continue
		}

		ranked = append(ranked, chair)
	}

	return ranked
}

func CurrentEpoch(now time.Time, rotationPeriod time.Duration) int64 {
	if rotationPeriod <= 0 {
		return 0
	}

	return now.UnixNano() / rotationPeriod.Nanoseconds()
}
