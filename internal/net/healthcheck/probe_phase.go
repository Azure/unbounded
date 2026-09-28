// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
	"time"
)

// Directional identities spread both a node's outgoing probes and the probes
// arriving at one peer from different nodes. No process-global RNG is needed.
func peerProbePhaseSeed(local, remote string) uint64 {
	key := make([]byte, 0, 16+len(local)+len(remote))
	key = binary.BigEndian.AppendUint64(key, uint64(len(local)))
	key = append(key, local...)
	key = binary.BigEndian.AppendUint64(key, uint64(len(remote)))
	key = append(key, remote...)
	hash := sha256.Sum256(key)

	return binary.BigEndian.Uint64(hash[:8])
}

// probePhase scales a stable fraction into [1ns, interval], without overflow or
// floating-point rounding. Only the first probe is offset; steady ticks retain
// the configured interval. Manager validation guarantees interval is positive.
func (s *session) probePhase(interval time.Duration) time.Duration {
	high, _ := bits.Mul64(s.probePhaseSeed, uint64(interval))
	return time.Duration(high + 1)
}
