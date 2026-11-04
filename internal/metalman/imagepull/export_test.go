// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

// BufferStats returns the bytes currently held in prefetched chunks and the
// peak since the Puller was first used.
func BufferStats(p *Puller) (used, peak int64) { return p.state().budget.stats() }

// IndexedLayers returns the number of layer indexes cached by p.
func IndexedLayers(p *Puller) int { return p.state().indexes.len() }

// QuarantineLen returns the number of (mirror, blob) pairs p avoids.
func QuarantineLen(p *Puller) int { return p.state().quarantine.len() }

// MaxQuarantined is the quarantine size cap.
const MaxQuarantined = maxQuarantined

// QuarantineAdd records blob as corrupt on mirror in p's quarantine.
func QuarantineAdd(p *Puller, mirror, blob string) { p.state().quarantine.add(mirror, blob) }

// QuarantineHas reports whether p avoids blob on mirror.
func QuarantineHas(p *Puller, mirror, blob string) bool {
	return p.state().quarantine.has(mirror, blob)
}
