// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

// BufferStats returns the bytes currently held in prefetched chunks and the
// peak since the Puller was first used.
func BufferStats(p *Puller) (used, peak int64) { return p.state().budget.stats() }

// IndexedLayers returns the number of layer indexes cached by p.
func IndexedLayers(p *Puller) int { return p.state().indexes.len() }
