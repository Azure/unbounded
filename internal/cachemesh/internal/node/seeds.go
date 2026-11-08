// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

// Seeds appends the current join seeds to dst and returns it. They come
// from Config.Seeds, the Lease record (leader address and published
// seeds), and views and replies seen while joining; unresponsive seeds are
// dropped while at least one is kept. The list stops changing once this
// member is in a view.
func (n *Node) Seeds(dst []string) []string { return append(dst, n.seeds...) }
