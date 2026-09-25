// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// VersionRecord is the only persisted topology bookkeeping. Hashes cover
// canonical content excluding counters. No member history or publication bytes
// are stored. ResourceVersion CAS must precede installing a publication.
type VersionRecord struct {
	Cluster           wire.ClusterID         `json:"cluster"`
	Sequence          wire.Sequence          `json:"sequence,string"`
	MembershipVersion wire.MembershipVersion `json:"membership_version,string"`
	ContentHash       string                 `json:"content_hash"`
	MembershipHash    string                 `json:"membership_hash"`
}

// PreparedPublication will own immutable encoded bytes and their candidate
// version record. Install must reject zero/uncommitted values; only CommitVersion
// will mint an accepted value after a successful resource-version CAS.
type (
	PreparedPublication  struct{}
	CommittedPublication struct{}
)

// Publications owns only the current immutable publication, bounded poll
// admission, and one broadcast notification. Older state belongs to dataplanes.
type Publications struct {
	Limits Limits
}

func NewPublications(limits Limits) *Publications { return &Publications{Limits: limits} }

func (*Publications) Prepare(_ VersionRecord, _ string, _ AcceptedMembers, _ []wire.CacheDefinition) (*PreparedPublication, error) {
	return nil, pending("publications.prepare")
}

func (*Publications) Install(_ *CommittedPublication) error {
	return pending("publications.install")
}

func (*Publications) Current() (*CommittedPublication, error) {
	return nil, pending("publications.current")
}

// Wait admits one poll per node, rejects future cursors, and honors context
// cancellation/certificate expiration. It never allocates a publication per poll.
func (*Publications) Wait(_ context.Context, _ NodeIdentity, _ *wire.Sequence) (*CommittedPublication, error) {
	return nil, pending("publications.wait")
}

func (*Publications) Ready(_ *http.Request) error { return pending("publications.ready") }
