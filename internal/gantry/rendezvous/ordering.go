// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func (m *Manager) readOrder(round uint64) []string {
	return rankedNames(m.SlotNames(), func(name string) [sha256.Size]byte {
		return hashParts(m.peerID.String(), strconv.FormatUint(round, 10), name)
	})
}

func (m *Manager) claimOrder() []string {
	return rankedNames(m.SlotNames(), func(name string) [sha256.Size]byte {
		return hashParts(m.peerID.String(), name)
	})
}

func rankedNames(names []string, score func(string) [sha256.Size]byte) []string {
	type ranked struct {
		name  string
		score [sha256.Size]byte
	}

	ranks := make([]ranked, len(names))
	for i, name := range names {
		ranks[i] = ranked{name: name, score: score(name)}
	}

	sort.Slice(ranks, func(i, j int) bool {
		return string(ranks[i].score[:]) < string(ranks[j].score[:])
	})

	result := make([]string, len(ranks))
	for i, rank := range ranks {
		result[i] = rank.name
	}

	return result
}

func hashParts(parts ...string) [sha256.Size]byte {
	hash := sha256.New()

	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}

	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))

	return result
}

func metricOutcome(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "timeout"
	default:
		return "error"
	}
}
