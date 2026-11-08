// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/leader"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Endpoint is a control-plane address, as given in Config.Addr and
// Config.Seeds. Its format is up to the ControlTransport.
type Endpoint = string

// ControlTransport carries control-plane messages. Every message is a
// request answered by one response. Send delivers msg to the member at
// addr, where the transport must call that member's Mesh.ServeControl, and
// returns its response. Send must honor ctx (it carries the call timeout)
// and be safe for concurrent use. Neither msg nor the returned slice is
// retained by the other side after the call returns.
type ControlTransport interface {
	Send(ctx context.Context, addr Endpoint, msg []byte) ([]byte, error)
}

// NeighborWatcher learns which data-plane peers this member should keep
// connections to. OnNeighbors calls are delivered one at a time, in order,
// on a goroutine owned by Run, never while holding the Mesh lock, so the
// watcher may call any Mesh method. Removals are delayed by the neighbor
// linger time so in-flight requests can finish.
type NeighborWatcher interface {
	OnNeighbors(added, removed []PeerRef)
}

// NeighborFunc adapts a function to NeighborWatcher.
type NeighborFunc func(added, removed []PeerRef)

// OnNeighbors calls f.
func (f NeighborFunc) OnNeighbors(added, removed []PeerRef) { f(added, removed) }

// Hooks are optional metric callbacks. They run on control-plane
// goroutines and must not block; nil fields are skipped. Hooks that are
// documented to run under the Mesh lock must not call Mesh methods.
type Hooks struct {
	// OnView is called under the Mesh lock after every view change with
	// the new version and member count.
	OnView func(v Version, members int)
	// OnLeadership is called under the Mesh lock when this member starts
	// or stops publishing deltas as leader.
	OnLeadership func(leading bool, term uint64)
	// OnControlError is called when an outbound control call fails
	// (transport error, timeout, or the in-flight limit was reached).
	OnControlError func(addr Endpoint, err error)
	// OnLeaseError is called when a Lease operation fails with an error
	// other than a conflict or not-found.
	OnLeaseError func(op string, err error)
}

// ElectionConfig tunes leader election. See the field documentation; zero
// values select defaults.
type ElectionConfig = elect.Config

// HealthConfig tunes circuit breakers, outlier detection, latency
// tracking, and the hedge budget. Zero values select defaults.
type HealthConfig = health.Config

// LeaderTuning tunes the work of the elected leader. Zero values select
// defaults. Every member should use the same values, since any member may
// become leader.
type LeaderTuning struct {
	// MaxJoinsPerTick bounds admissions per leader tick. Default 256.
	MaxJoinsPerTick int
	// QuarantineFor is how long a new member stays Joining. Default 10s.
	QuarantineFor time.Duration
	// WarmupStep is the time between weight steps of a new Active member.
	// Default 20s.
	WarmupStep time.Duration
	// DrainFor is how long a leaving member stays Draining before removal.
	// Default 30s.
	DrainFor time.Duration
	// SuspicionWindow is how long suspicion reports count. Default 30s.
	SuspicionWindow time.Duration
	// ObserverCells is the number of distinct observer cells needed to
	// remove a suspected member. Default 3.
	ObserverCells int
	// DampingPercent is the suspected share above which removals slow down.
	// Default 10.
	DampingPercent int
	// MaxMovesPerTick caps optional partition moves per tick. Default 4096;
	// negative disables optional moves.
	MaxMovesPerTick int
	// MaxCellMovesPerTick caps cell balancing moves per tick. Default 32.
	MaxCellMovesPerTick int
	// HandoffEpochs and SlotReuseEpochs are cluster constants written in
	// the first view. Defaults 8 and 16.
	HandoffEpochs, SlotReuseEpochs uint32
	// MaxPending bounds each leader input queue. Default 4096.
	MaxPending int
}

// Tuning holds advanced knobs. Zero values select defaults.
type Tuning struct {
	// PingInterval and PingTimeout pace failure detection. Defaults 2s, 1s.
	PingInterval, PingTimeout time.Duration
	// SuspectAfter is the number of failed pings before a suspicion
	// report. Default 3.
	SuspectAfter int
	// SuspectRefresh repeats a suspicion report. Default 5s.
	SuspectRefresh time.Duration
	// JoinRetry paces joins, leaves, and catch-up from seeds. Default 2s.
	JoinRetry time.Duration
	// CallTimeout bounds control calls. Default 2s.
	CallTimeout time.Duration
	// NeighborLinger delays neighbor removals. Default 30s.
	NeighborLinger time.Duration
	// LeaderTick is the leader's delta cadence. Default 1s.
	LeaderTick time.Duration
	// TickInterval is the elector and health tick cadence. Default 1s.
	TickInterval time.Duration
	// EpochAdvance is the longest the runtime waits between ticks, which
	// bounds how stale the epoch read by Fresh can be. Default 100ms.
	EpochAdvance time.Duration
	// CatchUpMaxDeltas, DeltaLogSize, and MaxAhead bound catch-up batches,
	// the served delta log, and queued future deltas. Defaults 256, 512,
	// 64.
	CatchUpMaxDeltas, DeltaLogSize, MaxAhead int
	// MaxInFlight bounds concurrent outbound control calls; calls beyond
	// it fail at once and are retried by the protocol. Default 64.
	MaxInFlight int
	// LeaseTimeout bounds each Lease operation. Default
	// Election.OpTimeout (which defaults to Election.RetryPeriod).
	LeaseTimeout time.Duration

	Election ElectionConfig
	Health   HealthConfig
	Leader   LeaderTuning
}

// Config configures a Mesh. Zero values select defaults.
type Config struct {
	// ID uniquely names this member, for example the Kubernetes node name.
	// Required.
	ID string
	// Addr is this member's control endpoint, where other members'
	// transports reach ServeControl. Required.
	Addr Endpoint
	// Transport sends control messages. Required.
	Transport ControlTransport
	// Incarnation distinguishes restarts of the same ID. Zero picks a
	// random nonzero value.
	Incarnation uint64
	// Zone is informational (reported in the view).
	Zone string
	// Seeds are control endpoints of existing members to join through.
	// While this member is not in a view, Route sends requests through the
	// seeds (KindGateway); see Mesh.Route.
	Seeds []Endpoint
	// Lease is the store holding the leader Lease, for example a
	// *kubelease.Store. It may be nil: such a member can only join through
	// Seeds, never bootstraps a cluster, and never becomes leader. At least
	// one member of a cluster needs a Lease.
	Lease LeaseStore
	// Neighbors, when set, is told which data-plane connections to keep.
	Neighbors NeighborWatcher
	// Logger receives control-plane logs. Default slog.Default().
	Logger *slog.Logger
	// Hooks are metric callbacks.
	Hooks Hooks

	// PartitionBits is log2 of the partition count. Must match the
	// cluster. Default 20.
	PartitionBits uint8
	// CellSize is the target members per grid cell. Default 4.
	CellSize int
	// MaxNeighbors bounds data-plane neighbors per member. Default 256.
	MaxNeighbors int
	// EpochPeriod is the epoch length the leader publishes. Default 15s.
	EpochPeriod time.Duration

	// Tuning holds advanced knobs.
	Tuning Tuning

	// initial is a prebuilt view for tests and benchmarks.
	initial *view.View
}

// Defaults.
const (
	DefaultMaxInFlight  = 64
	DefaultEpochAdvance = 100 * time.Millisecond
)

func (c *Config) validate() error {
	var errs []error

	if c.ID == "" {
		errs = append(errs, errors.New("ID is required"))
	}

	if c.Addr == "" {
		errs = append(errs, errors.New("addr is required"))
	}

	if c.Transport == nil {
		errs = append(errs, errors.New("transport is required"))
	}

	if c.PartitionBits != 0 && !placement.ValidBits(c.PartitionBits) {
		errs = append(errs, fmt.Errorf("invalid PartitionBits %d", c.PartitionBits))
	}

	neg := func(name string, d time.Duration) {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative", name))
		}
	}
	t := &c.Tuning
	neg("EpochPeriod", c.EpochPeriod)
	neg("PingInterval", t.PingInterval)
	neg("PingTimeout", t.PingTimeout)
	neg("SuspectRefresh", t.SuspectRefresh)
	neg("JoinRetry", t.JoinRetry)
	neg("CallTimeout", t.CallTimeout)
	neg("NeighborLinger", t.NeighborLinger)
	neg("LeaderTick", t.LeaderTick)
	neg("TickInterval", t.TickInterval)
	neg("EpochAdvance", t.EpochAdvance)
	neg("LeaseTimeout", t.LeaseTimeout)

	if c.CellSize < 0 || c.MaxNeighbors < 0 || t.SuspectAfter < 0 || t.MaxInFlight < 0 ||
		t.CatchUpMaxDeltas < 0 || t.DeltaLogSize < 0 || t.MaxAhead < 0 {
		errs = append(errs, errors.New("counts must not be negative"))
	}

	if err := t.Election.Validate(); err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("cachemesh: invalid config: %w", err)
	}

	return nil
}

func (c *Config) nodeConfig() node.Config {
	t := &c.Tuning
	lt := &t.Leader

	return node.Config{
		ID:            c.ID,
		Incarnation:   c.Incarnation,
		Addr:          c.Addr,
		Zone:          c.Zone,
		Seeds:         append([]string(nil), c.Seeds...),
		PartitionBits: c.PartitionBits,
		Elect:         t.Election,
		Health:        t.Health,
		Leader: leader.Config{
			MaxJoinsPerTick:     lt.MaxJoinsPerTick,
			QuarantineFor:       lt.QuarantineFor,
			WarmupStep:          lt.WarmupStep,
			DrainFor:            lt.DrainFor,
			SuspicionWindow:     lt.SuspicionWindow,
			ObserverCells:       lt.ObserverCells,
			DampingPercent:      lt.DampingPercent,
			MaxMoves:            lt.MaxMovesPerTick,
			MaxCellMovesPerTick: lt.MaxCellMovesPerTick,
			CellSize:            c.CellSize,
			MaxNeighbors:        c.MaxNeighbors,
			SlotReuseEpochs:     lt.SlotReuseEpochs,
			HandoffEpochs:       lt.HandoffEpochs,
			MaxPending:          lt.MaxPending,
		},
		EpochPeriod:      c.EpochPeriod,
		PingInterval:     t.PingInterval,
		PingTimeout:      t.PingTimeout,
		SuspectAfter:     t.SuspectAfter,
		SuspectRefresh:   t.SuspectRefresh,
		JoinRetry:        t.JoinRetry,
		CatchUpMaxDeltas: t.CatchUpMaxDeltas,
		CallTimeout:      t.CallTimeout,
		DeltaLogSize:     t.DeltaLogSize,
		NeighborLinger:   t.NeighborLinger,
		LeaderTick:       t.LeaderTick,
		TickInterval:     t.TickInterval,
		MaxAhead:         t.MaxAhead,
		Initial:          c.initial,
	}
}
