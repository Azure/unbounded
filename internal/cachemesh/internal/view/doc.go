// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package view holds the immutable membership view that every cachemesh
// member routes with: members by slot, the grid shape and cell index, the
// partition table, the epoch anchor, and keyspace generations. Views change
// only by applying leader-issued deltas, and each view is identified by a
// Version and a content Digest. See designs/cachemesh.md, "Membership view".
package view
