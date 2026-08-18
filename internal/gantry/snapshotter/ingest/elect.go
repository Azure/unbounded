// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Elector decides how eagerly this node should ingest a layer.
//
// A layer only needs to be written once for the whole cluster: every other node
// reads it out of the shared image device. Without some form of election, a
// thousand nodes missing on the same cold image would each build the same EROFS
// blob and each burn a segment reservation for it, which is exactly the
// duplicated work this snapshotter exists to remove.
//
// Election is expressed as a delay rather than a yes or no, because a hard
// election is wrong in the case that matters most. Consider a single node
// pulling a brand new forty layer image. Under a hard rendezvous election that
// node would own roughly one layer in forty and the other thirty nine would
// never be ingested by anybody, because no other node has any reason to look at
// that image. A delay makes the favoured node ingest immediately and every
// other node ingest only if the favoured node did not, which is correct whether
// one node or a thousand are pulling.
//
// Delay is deliberately not an error-returning call. There is no useful failure
// mode: if membership is unknown the honest answer is zero, since a duplicate
// blob costs one segment's worth of space and a layer that never lands costs
// every future pod on every node.
type Elector interface {
	// Delay is how long to wait, after the local unpack, before ingesting.
	// Zero means ingest immediately.
	Delay(layer digest.Digest) time.Duration
}

// Immediate ingests without waiting. It is the right choice for a single node
// and for tests.
type Immediate struct{}

// Delay always returns zero.
func (Immediate) Delay(digest.Digest) time.Duration { return 0 }

// Fixed waits the same amount for every layer.
type Fixed time.Duration

// Delay returns the fixed wait.
func (f Fixed) Delay(digest.Digest) time.Duration { return time.Duration(f) }

// DefaultStep is how much later each successive rank ingests.
//
// It has to be comfortably longer than one erofs build plus one segment write,
// or the second ranked node will start building before the first has published
// and the deduplication is lost. It also has to be short enough that a cold
// image on a single node is not stalled behind it. Tens of seconds is the band
// where both hold for the layer sizes that matter.
const DefaultStep = 30 * time.Second

// DefaultMaxDelay caps the wait for a deeply ranked or unknown node.
const DefaultMaxDelay = 10 * time.Minute

// HRWOptions configures an HRW elector.
type HRWOptions struct {
	// Self is this node's gantry node ID. Required.
	Self ifaces.NodeID

	// Members returns the current membership view. It is called on every
	// election, so it must be cheap: gantry's informer-backed view already
	// is. Required.
	Members func() []ifaces.Node

	// Step is the delay added per rank. Defaults to DefaultStep.
	Step time.Duration

	// MaxDelay caps the wait. Defaults to DefaultMaxDelay.
	MaxDelay time.Duration

	// Jitter is the fraction of Step added at random, in [0,1]. It keeps a
	// thousand equally ranked nodes from waking together when the favoured
	// node has died. Defaults to 0.25.
	Jitter float64

	// Scope narrows the candidate set, for example to this node's zone.
	Scope hrw.Scope

	// Zone is this node's zone, used when Scope is zone-local.
	Zone string
}

// HRW ranks nodes by rendezvous hash and turns the rank into a delay.
//
// Rendezvous hashing is the right ranking function here because every node
// derives the same order from the same membership view with no coordination,
// and because it is the same rule gantry already uses to pick the designated
// origin puller for a digest. The node that fetched the layer bytes is
// therefore usually the node that ingests them, which is the cheapest possible
// assignment.
type HRW struct {
	opts HRWOptions
}

// NewHRW builds an HRW elector.
func NewHRW(opts HRWOptions) (*HRW, error) {
	if opts.Self == "" {
		return nil, errNoSelf
	}

	if opts.Members == nil {
		return nil, errNoMembers
	}

	if opts.Step <= 0 {
		opts.Step = DefaultStep
	}

	if opts.MaxDelay <= 0 {
		opts.MaxDelay = DefaultMaxDelay
	}

	if opts.Jitter < 0 || opts.Jitter > 1 {
		return nil, errors.New("ingest: jitter must be in [0,1]")
	}

	if opts.Jitter == 0 {
		opts.Jitter = 0.25
	}

	return &HRW{opts: opts}, nil
}

// Delay returns this node's wait for the layer.
//
// A node that is not in the membership view at all waits the cap: it is either
// starting up or draining, and in both cases it should defer to the nodes that
// are steady, but it must not defer forever in case it is the only node with
// the bytes.
func (h *HRW) Delay(layer digest.Digest) time.Duration {
	if layer.IsZero() {
		return 0
	}

	members := h.opts.Members()
	if len(members) == 0 {
		return 0
	}

	candidates := hrw.Candidates(members, h.opts.Scope, h.opts.Zone)
	if len(candidates) == 0 {
		return 0
	}

	rank := hrw.RankOf(candidates, h.opts.Self, layer)
	if rank < 0 {
		return h.opts.MaxDelay
	}

	if rank == 0 {
		return 0
	}

	d := time.Duration(rank) * h.opts.Step
	d += time.Duration(rand.Float64() * h.opts.Jitter * float64(h.opts.Step)) //nolint:gosec // jitter, not a secret

	if d > h.opts.MaxDelay {
		d = h.opts.MaxDelay
	}

	return d
}
